package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const oidcCookieAge = 5 * time.Minute

type oidcLogin struct {
	subjects map[string]struct{}
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	verify   func(*http.Request, string, string) (string, error)
}

// SetOIDC enables explicitly pinned IdP identities for this single-admin app.
// Discovery errors fail startup rather than leaving a misleading SSO button.
func (s *Server) SetOIDC(ctx context.Context, issuer, clientID, clientSecret, baseURL, subjects string) error {
	allowed := make(map[string]struct{})
	for _, id := range strings.Split(subjects, ",") {
		if id == "" || strings.TrimSpace(id) != id {
			return errors.New("OIDC subjects must be nonempty and exact")
		}
		allowed[id] = struct{}{}
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return err
	}
	s.oidc = &oidcLogin{
		subjects: allowed,
		oauth:    oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, RedirectURL: strings.TrimRight(baseURL, "/") + "/auth/oidc/callback", Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID}},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}
	return nil
}

func (s *Server) oidcCookie(w http.ResponseWriter, r *http.Request, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/auth/oidc/", MaxAge: age, Expires: time.Now().Add(time.Duration(age) * time.Second), HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode})
}

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	state, err := randomToken(32)
	if err != nil {
		http.Error(w, errMsgServerErr, 500)
		return
	}
	nonce, err := randomToken(32)
	if err != nil {
		http.Error(w, errMsgServerErr, 500)
		return
	}
	s.oidcCookie(w, r, "idler_oidc_state", state, int(oidcCookieAge.Seconds()))
	s.oidcCookie(w, r, "idler_oidc_nonce", nonce, int(oidcCookieAge.Seconds()))
	verifier := oauth2.GenerateVerifier()
	s.oidcCookie(w, r, "idler_oidc_pkce", verifier, int(oidcCookieAge.Seconds()))
	http.Redirect(w, r, s.oidc.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	state, e1 := r.Cookie("idler_oidc_state")
	nonce, e2 := r.Cookie("idler_oidc_nonce")
	pkce, e3 := r.Cookie("idler_oidc_pkce")
	s.oidcCookie(w, r, "idler_oidc_state", "", -1)
	s.oidcCookie(w, r, "idler_oidc_nonce", "", -1)
	s.oidcCookie(w, r, "idler_oidc_pkce", "", -1)
	if e1 != nil || e2 != nil || e3 != nil || state.Value == "" || nonce.Value == "" || pkce.Value == "" || subtle.ConstantTimeCompare([]byte(state.Value), []byte(r.URL.Query().Get("state"))) != 1 || r.URL.Query().Get("code") == "" {
		http.Error(w, "invalid OIDC callback", http.StatusForbidden)
		return
	}
	var subject string
	var err error
	if s.oidc.verify != nil {
		subject, err = s.oidc.verify(r, r.URL.Query().Get("code"), nonce.Value)
	} else {
		subject, err = s.oidc.verifyCode(r.Context(), r.URL.Query().Get("code"), nonce.Value, pkce.Value)
	}
	_, authorized := s.oidc.subjects[subject]
	if err != nil || subject == "" || !authorized {
		slog.Warn("OIDC login rejected", "reason", "identity not authorized or token verification failed")
		http.Error(w, "OIDC identity not authorized", http.StatusForbidden)
		return
	}
	var id int
	var hash string
	if err := s.db.QueryRowContext(r.Context(), "SELECT id,password_hash FROM users LIMIT 1").Scan(&id, &hash); err != nil {
		http.Error(w, errMsgServerErr, 500)
		return
	}
	if err := s.createSession(w, r, id, hash); err != nil {
		http.Error(w, errMsgServerErr, 500)
		return
	}
	slog.Info("OIDC login authenticated", "from", s.clientIP(r))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (o *oidcLogin) verifyCode(ctx context.Context, code, nonce, pkce string) (string, error) {
	tok, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkce))
	if err != nil {
		return "", err
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return "", errors.New("missing ID token")
	}
	id, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return "", errors.New("OIDC nonce mismatch")
	}
	return id.Subject, nil
}
