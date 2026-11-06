package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOIDCLoginButtonAfterPasswordForm(t *testing.T) {
	ts, _, app := newTestServerFull(t)
	defer ts.Close()
	app.oidc = &oidcLogin{subjects: map[string]struct{}{"owner-admin": {}}}
	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	form := strings.Index(page, "</form>")
	button := strings.Index(page, "Sign in with SSO")
	if form < 0 || button < form || !strings.Contains(page[form:button], "auth-divider") {
		t.Fatal("SSO button should follow the password form with a divider")
	}
}

func TestOIDCDisabledByDefault(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	r, err := http.Get(ts.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled OIDC status = %d", r.StatusCode)
	}
}

func TestOIDCUnmatchedSubjectCannotCreateSession(t *testing.T) {
	ts, _, s := newTestServerFull(t)
	defer ts.Close()
	s.oidc = &oidcLogin{subjects: map[string]struct{}{"pinned": {}}, verify: func(_ *http.Request, code, nonce string) (string, error) { return "unmatched", nil }}
	req := httptest.NewRequest("GET", "/auth/oidc/callback?code=valid&state=state", nil)
	req.AddCookie(&http.Cookie{Name: "idler_oidc_state", Value: "state"})
	req.AddCookie(&http.Cookie{Name: "idler_oidc_nonce", Value: "nonce"})
	req.AddCookie(&http.Cookie{Name: "idler_oidc_pkce", Value: "verifier"})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || strings.Contains(rr.Header().Get("Set-Cookie"), "idler_session=") {
		t.Fatalf("unmatched subject granted session: %d", rr.Code)
	}
}

func TestOIDCStartAndCallbackGuards(t *testing.T) {
	ts, _, app := newTestServerFull(t)
	defer ts.Close()
	app.oidc = &oidcLogin{subjects: map[string]struct{}{"pinned": {}}, verify: func(_ *http.Request, _, _ string) (string, error) { return "pinned", nil }}
	client := newClient(t)
	start, err := client.Get(ts.URL + "/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	if start.StatusCode != http.StatusFound {
		t.Fatalf("start: %d", start.StatusCode)
	}
	state := start.Header.Get("Set-Cookie")
	_ = state
	cookies := client.Jar.Cookies(mustURL(t, ts.URL+"/auth/oidc/callback"))
	vals := map[string]string{}
	for _, c := range cookies {
		vals[c.Name] = c.Value
	}
	if vals["idler_oidc_state"] == "" || vals["idler_oidc_nonce"] == "" || vals["idler_oidc_pkce"] == "" {
		t.Fatal("state and nonce required")
	}
	for _, tc := range []struct {
		name, state, nonce, subject string
		want                        int
	}{
		{"wrong state", "wrong", vals["idler_oidc_nonce"], "pinned", 403},
		{"missing nonce", vals["idler_oidc_state"], "", "pinned", 403},
		{"correct", vals["idler_oidc_state"], vals["idler_oidc_nonce"], "pinned", 303},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/auth/oidc/callback?code=ok&state="+url.QueryEscape(tc.state), nil)
			req.AddCookie(&http.Cookie{Name: "idler_oidc_state", Value: vals["idler_oidc_state"]})
			req.AddCookie(&http.Cookie{Name: "idler_oidc_pkce", Value: vals["idler_oidc_pkce"]})
			if tc.nonce != "" {
				req.AddCookie(&http.Cookie{Name: "idler_oidc_nonce", Value: tc.nonce})
			}
			rr := httptest.NewRecorder()
			app.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status %d want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestOIDCVerifyCodeChecksSignedTokenAndNonce(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "test", Algorithm: "RS256", Use: "sig"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			nonce := r.FormValue("nonce")
			_ = nonce
			claims := map[string]any{"iss": issuer, "aud": "client", "sub": "pinned", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": "expected"}
			b, _ := json.Marshal(claims)
			j, _ := signer.Sign(b)
			raw, _ := j.CompactSerialize()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"access_token": "access", "token_type": "Bearer", "id_token": raw})
		}
	}))
	defer idp.Close()
	issuer = idp.URL
	provider, err := oidc.NewProvider(context.Background(), issuer)
	if err != nil {
		t.Fatal(err)
	}
	o := &oidcLogin{oauth: oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: provider.Endpoint()}, verifier: provider.Verifier(&oidc.Config{ClientID: "client"})}
	if sub, err := o.verifyCode(context.Background(), "ok", "wrong", "verifier"); err == nil || sub != "" {
		t.Fatal("nonce mismatch accepted")
	}
	if sub, err := o.verifyCode(context.Background(), "ok", "expected", "verifier"); err != nil || sub != "pinned" {
		t.Fatalf("valid token failed: %v", err)
	}
}

func TestOIDCStartIncludesNonceAndState(t *testing.T) {
	_, _, s := newTestServerFull(t)
	s.oidc = &oidcLogin{oauth: oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://auth.example/authorize"}, RedirectURL: "https://idler.example/auth/oidc/callback", Scopes: []string{oidc.ScopeOpenID}}}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/auth/oidc/start", nil))
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != 302 || u.Query().Get("state") == "" || u.Query().Get("nonce") == "" || u.Query().Get("scope") != "openid" {
		t.Fatalf("bad auth redirect: %d", rr.Code)
	}
}

func TestOIDCStartAndCallbackUsesPKCE(t *testing.T) {
	_, _, app := newTestServerFull(t)
	app.oidc = &oidcLogin{subjects: map[string]struct{}{"pinned": {}}, oauth: oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://auth.example/authorize"}, RedirectURL: "https://idler.example/auth/oidc/callback"}, verify: func(_ *http.Request, _, _ string) (string, error) { return "pinned", nil }}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/auth/oidc/start", nil))
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("code_challenge") == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("OIDC auth code flow must use PKCE")
	}
}
