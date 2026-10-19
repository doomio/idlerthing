// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package web implements the HTTP layer: routes, middleware, auth, and templates.
package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
)

//go:embed assets/static assets/templates
var assetsFS embed.FS

// staticFS holds the embedded static assets subtree.
var staticFS = mustSub(assetsFS, "assets/static")

// mustSub returns the dir subtree of fsys, panicking if it does not exist
// (the embedded asset layout is a compile-time invariant).
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Server is the web application. Construct with New, serve with Handler.
type Server struct {
	db         *sql.DB
	tmpl       *templates
	limit      *rateLimiter
	emailLimit *rateLimiter
	blockLog   *rateLimiter // throttles the 'rate-limited' journal line
	pingLimit  *rateLimiter
	knownIPs   *knownIPs // sources that have authenticated before (see handleLoginPost)
	servers    *model.ServerStore
	catalogs   *model.CatalogStore
	pricings   *model.PricingStore
	rates      *pricing.Rates
	whoisURL   string // injectable for tests; default ipwho.is
	dash       *dashboardCache
	secret     []byte // yabs ingest HMAC key
	prom       *promCache
	uptime     uptimeCache
	livemon    liveMonCache

	publicCache     publicCacheEntry
	behindTLSProxy  bool
	allowHTTPIngest bool           // IDLER_BEHIND_TLS_PROXY
	baseURL         string         // IDLER_BASE_URL ("" = derive from request)
	whoisRate       whoisRateLimit // per-server whois throttle (fresh in tests)

	// pmapiURL overrides the PulsedMedia feed endpoint (IDLER_PMAPI_URL).
	// Empty uses the built-in URL; tests point it at a mock server.
	pmapiURL string
	sshPoll  func(context.Context, *model.Seedbox) (sshPollResult, error)
	qbtPoll  func(context.Context, *model.Seedbox) (string, error)

	// pmSyncing serialises fleet syncs so a burst of clicks cannot launch
	// several fetches at once (the provider rate-limits by key). Per-Server
	// like every other mutable state here — package globals would let one
	// test's in-flight sync block an unrelated Server in another.
	pmSyncMu  sync.Mutex
	pmSyncing bool

	// bootstrapFile is the path holding the generated first-run admin
	// password (see main.go's seedAdmin). It is deleted as soon as anyone
	// signs in successfully, so the credential stops sitting on disk.
	bootstrapFile string

	// AGPL §13 texts, injected from package main (go:embed cannot reach the
	// repo root from here) — see legal.go.
	license    []byte
	thirdParty []byte
	version    string
}

// New creates a Server backed by db.
func New(db *sql.DB) (*Server, error) {
	t, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &Server{
		db:         db,
		tmpl:       t,
		limit:      newRateLimiter(10, time.Minute),
		emailLimit: newRateLimiter(10, time.Minute),
		// One line per source per window (see Server.logBlocked).
		blockLog:  newRateLimiter(1, time.Minute),
		pingLimit: newRateLimiter(10, time.Minute),
		knownIPs:  newKnownIPs(64),
		servers:   &model.ServerStore{DB: db},
		catalogs:  &model.CatalogStore{DB: db},
		pricings:  &model.PricingStore{DB: db},
		rates:     pricing.NewRates(),
		dash:      &dashboardCache{},
		secret:    secret,
		prom:      &promCache{},
		sshPoll:   pollSeedboxLoad,
		qbtPoll:   pollQBT,
	}, nil
}

// SetSecret overrides the yabs ingest HMAC key.
func (s *Server) SetSecret(secret []byte) {
	s.secret = secret
}

// SetBootstrapPasswordFile points the login path at the file holding the
// generated first-run admin password; it is removed on the first successful
// sign-in. Empty (the default) disables the cleanup.
func (s *Server) SetBootstrapPasswordFile(path string) {
	s.bootstrapFile = path
}

// SetBehindTLSProxy marks the app as behind an HTTPS-terminating proxy.
func (s *Server) SetBehindTLSProxy(behind bool) {
	s.behindTLSProxy = behind
}

// SetPMAPIURL points the PulsedMedia feed client at a different endpoint.
// Empty (the default) uses the built-in customer-API URL.
func (s *Server) SetPMAPIURL(url string) {
	s.pmapiURL = url
}

// SetAllowHTTPIngest permits plain-http ingest URLs on LAN hosts.
func (s *Server) SetAllowHTTPIngest(allow bool) {
	s.allowHTTPIngest = allow
}

// SetBaseURL sets the external base URL for the yabs ingest command.
// Only http(s) URLs without single-quote characters are accepted.
func (s *Server) SetBaseURL(u string) {
	u = strings.TrimRight(u, "/")
	if strings.Contains(u, "'") ||
		!(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
		u = ""
	}
	s.baseURL = u
}

// Handler returns the root handler with the full middleware chain:
// recover → security headers → session loading → routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public routes.
	mux.HandleFunc("GET /static/accent.css", s.handleAccentCSS)
	mux.HandleFunc("GET /static/favicon.svg", s.handleFaviconSVG)
	mux.HandleFunc("GET /favicon.ico", s.handleFaviconICO)
	mux.Handle("GET /static/", s.withCacheHeaders(http.StripPrefix("/static/", http.FileServerFS(staticFS))))
	// AGPL §13: the license, the third-party notices, and the Corresponding
	// Source offer must reach every network user, so these sit OUTSIDE
	// requireAuth alongside the other public routes.
	mux.HandleFunc("GET /license", s.serveLegalText(func() []byte { return s.license }, "license text unavailable"))
	mux.HandleFunc("GET /third-party-licenses", s.serveLegalText(func() []byte { return s.thirdParty }, "third-party notices unavailable"))
	mux.HandleFunc("GET /source", s.handleSource)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// Bounded: Ping would otherwise queue behind the single connection
		// for as long as a slow write takes.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.PingContext(ctx); err != nil {
			http.Error(w, "unhealthy\n", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /login", s.handleLoginGet)
	mux.HandleFunc("POST /login", s.handleLoginPost)

	// Protected routes (session required, CSRF enforced on unsafe methods).
	mux.Handle("GET /{$}", s.protect(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("GET /", s.protect(http.HandlerFunc(s.handleNotFound)))
	mux.Handle("POST /logout", s.protect(http.HandlerFunc(s.handleLogout)))
	mux.Handle("POST /prefs/theme", s.protect(http.HandlerFunc(s.handleThemePref)))
	mux.Handle("POST /prefs/servers-cols", s.protect(http.HandlerFunc(s.handleServerColsPref)))
	mux.Handle("POST /prefs/short-hostnames", s.protect(http.HandlerFunc(s.handleShortHostnamesPref)))

	// Catalogs.
	mux.Handle("GET /catalogs/{kind}", s.protect(http.HandlerFunc(s.handleCatalogList)))
	mux.Handle("POST /catalogs/{kind}", s.protect(http.HandlerFunc(s.handleCatalogCreate)))
	mux.Handle("POST /catalogs/{kind}/{id}/update", s.protect(http.HandlerFunc(s.handleCatalogUpdate)))
	mux.Handle("POST /catalogs/{kind}/{id}/key", s.protect(http.HandlerFunc(s.handleProviderKey)))
	mux.Handle("POST /catalogs/{kind}/{id}/delete", s.protect(http.HandlerFunc(s.handleCatalogDelete)))

	// Servers.
	mux.Handle("GET /servers", s.protect(http.HandlerFunc(s.handleServerList)))
	mux.Handle("GET /servers/new", s.protect(http.HandlerFunc(s.handleServerNew)))
	mux.Handle("POST /servers", s.protect(http.HandlerFunc(s.handleServerCreate)))
	mux.Handle("GET /servers/{id}", s.protect(http.HandlerFunc(s.handleServerDetail)))
	mux.Handle("GET /servers/{id}/edit", s.protect(http.HandlerFunc(s.handleServerEdit)))
	mux.Handle("POST /servers/{id}/update", s.protect(http.HandlerFunc(s.handleServerUpdate)))
	mux.Handle("POST /servers/{id}/delete", s.protect(http.HandlerFunc(s.handleServerDelete)))

	// Shared + reseller hosting.
	for _, cfg := range []*hostingConfig{s.sharedConfig(), s.resellerConfig()} {
		sec := s.hostingSection(cfg)
		mux.Handle("GET "+cfg.base, s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleSectionList(w, r, sec)
		})))
		mux.Handle("GET "+cfg.base+"/new", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingNew(w, r, cfg)
		})))
		mux.Handle("POST "+cfg.base, s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingCreate(w, r, cfg)
		})))
		mux.Handle("GET "+cfg.base+"/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleSectionDetail(w, r, sec)
		})))
		mux.Handle("GET "+cfg.base+"/{id}/edit", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingEdit(w, r, cfg)
		})))
		mux.Handle("POST "+cfg.base+"/{id}/update", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingUpdate(w, r, cfg)
		})))
		mux.Handle("POST "+cfg.base+"/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleSectionDelete(w, r, sec)
		})))
	}

	// Seedboxes.
	seedboxSec := s.seedboxSection()
	mux.Handle("GET /seedboxes", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionList(w, r, seedboxSec) })))
	// Registered before /seedboxes/{id}: the literal wins, so "fleet" is not
	// parsed as an id.
	mux.Handle("GET /seedboxes/fleet", s.protect(http.HandlerFunc(s.handleSeedboxFleet)))
	mux.Handle("POST /seedboxes/fleet/sync", s.protect(http.HandlerFunc(s.handleSeedboxFleetSync)))
	mux.Handle("POST /seedboxes/{id}/poll", s.protect(http.HandlerFunc(s.handleSeedboxPoll)))
	mux.Handle("POST /seedboxes/{id}/poll-qbt", s.protect(http.HandlerFunc(s.handleQBTPoll)))
	mux.Handle("GET /seedboxes/new", s.protect(http.HandlerFunc(s.handleSeedboxNew)))
	mux.Handle("POST /seedboxes", s.protect(http.HandlerFunc(s.handleSeedboxCreate)))
	mux.Handle("GET /seedboxes/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDetail(w, r, seedboxSec) })))
	mux.Handle("GET /seedboxes/{id}/edit", s.protect(http.HandlerFunc(s.handleSeedboxEdit)))
	mux.Handle("POST /seedboxes/{id}/update", s.protect(http.HandlerFunc(s.handleSeedboxUpdate)))
	mux.Handle("POST /seedboxes/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDelete(w, r, seedboxSec) })))

	// Domains.
	domainSec := s.domainSection()
	mux.Handle("GET /domains", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionList(w, r, domainSec) })))
	mux.Handle("GET /domains/new", s.protect(http.HandlerFunc(s.handleDomainNew)))
	mux.Handle("POST /domains", s.protect(http.HandlerFunc(s.handleDomainCreate)))
	mux.Handle("GET /domains/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDetail(w, r, domainSec) })))
	mux.Handle("GET /domains/{id}/edit", s.protect(http.HandlerFunc(s.handleDomainEdit)))
	mux.Handle("POST /domains/{id}/update", s.protect(http.HandlerFunc(s.handleDomainUpdate)))
	mux.Handle("POST /domains/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDelete(w, r, domainSec) })))

	// Misc services.
	miscSec := s.miscSection()
	mux.Handle("GET /misc", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionList(w, r, miscSec) })))
	mux.Handle("GET /misc/new", s.protect(http.HandlerFunc(s.handleMiscNew)))
	mux.Handle("POST /misc", s.protect(http.HandlerFunc(s.handleMiscCreate)))
	mux.Handle("GET /misc/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDetail(w, r, miscSec) })))
	mux.Handle("GET /misc/{id}/edit", s.protect(http.HandlerFunc(s.handleMiscEdit)))
	mux.Handle("POST /misc/{id}/update", s.protect(http.HandlerFunc(s.handleMiscUpdate)))
	mux.Handle("POST /misc/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDelete(w, r, miscSec) })))

	// Labels, notes, IPs, DNS.
	mux.Handle("POST /labels/assign", s.protect(http.HandlerFunc(s.handleLabelAssign)))
	mux.Handle("POST /labels/unassign", s.protect(http.HandlerFunc(s.handleLabelUnassign)))
	mux.Handle("GET /notes", s.protect(http.HandlerFunc(s.handleNotesIndex)))
	mux.Handle("POST /notes", s.protect(http.HandlerFunc(s.handleNoteCreate)))
	mux.Handle("POST /notes/{id}/delete", s.protect(http.HandlerFunc(s.handleNoteDelete)))
	mux.Handle("GET /ips", s.protect(http.HandlerFunc(s.handleIPsIndex)))
	mux.Handle("POST /ips", s.protect(http.HandlerFunc(s.handleIPCreate)))
	mux.Handle("POST /ips/{id}/delete", s.protect(http.HandlerFunc(s.handleIPDelete)))
	mux.Handle("POST /ips/{id}/whois", s.protect(http.HandlerFunc(s.handleIPWhois)))
	mux.Handle("GET /dns", s.protect(http.HandlerFunc(s.handleDNSIndex)))
	mux.Handle("POST /dns", s.protect(http.HandlerFunc(s.handleDNSCreate)))
	mux.Handle("GET /dns/{id}/edit", s.protect(http.HandlerFunc(s.handleDNSEdit)))
	mux.Handle("POST /dns/{id}/update", s.protect(http.HandlerFunc(s.handleDNSUpdate)))
	mux.Handle("POST /dns/{id}/delete", s.protect(http.HandlerFunc(s.handleDNSDelete)))

	// Settings.
	mux.Handle("GET /settings", s.protect(http.HandlerFunc(s.handleSettingsGet)))
	mux.Handle("POST /settings", s.protect(http.HandlerFunc(s.handleSettingsUpdate)))
	mux.Handle("POST /settings/account", s.protect(http.HandlerFunc(s.handleSettingsAccount)))
	mux.Handle("POST /settings/prometheus/test", s.protect(http.HandlerFunc(s.handlePrometheusTest)))

	// Export.
	mux.Handle("GET /export/json", s.protect(http.HandlerFunc(s.handleExportJSON)))
	mux.Handle("GET /export/json/{type}", s.protect(http.HandlerFunc(s.handleExportJSON)))
	mux.Handle("GET /export/csv", s.protect(http.HandlerFunc(s.handleExportCSV)))

	// JSON API (token auth, no session/CSRF). Method-qualified patterns so
	// they don't conflict with the "GET /" catch-all.
	apiHandler := s.apiMux()
	mux.Handle("GET /api/", apiHandler)
	mux.Handle("POST /api/", apiHandler)
	mux.Handle("PUT /api/", apiHandler)
	mux.Handle("DELETE /api/", apiHandler)

	// Public yabs ingest (signature-authed, no session/CSRF/token) and the
	// public servers page.
	mux.HandleFunc("POST /api/yabs/{id}", s.handleYABSIngest)
	mux.HandleFunc("GET /public", s.handlePublic)

	// YABS views + ping tool.
	mux.Handle("GET /yabs", s.protect(http.HandlerFunc(s.handleYABSIndex)))
	mux.Handle("GET /servers/{id}/yabs", s.protect(http.HandlerFunc(s.handleServerYABS)))
	mux.Handle("GET /servers/{id}/yabs/{yid}", s.protect(http.HandlerFunc(s.handleServerYABSDetail)))
	mux.Handle("POST /servers/{id}/yabs/{yid}/delete", s.protect(http.HandlerFunc(s.handleServerYABSDelete)))
	mux.Handle("POST /tools/ping", s.protect(http.HandlerFunc(s.handlePing)))

	// Fallback target for every destructive form whose confirmation only htmx
	// can provide (see handleJSRequired).
	mux.Handle("POST /js-required", s.protect(http.HandlerFunc(s.handleJSRequired)))
	// Re-rendered after an in-place htmx swap so the sidebar counts catch up.
	mux.Handle("GET /partials/counts", s.protect(http.HandlerFunc(s.handleSidebarCounts)))

	return s.recoverMiddleware(s.securityHeaders(s.loadSession(mux)))
}

// protect wraps a handler with session auth + CSRF protection.
func (s *Server) protect(next http.Handler) http.Handler {
	return s.requireAuth(s.csrfProtect(next))
}

// cacheStatusWriter injects the immutable Cache-Control only when the
// response is 2xx — a 404 for a missing asset must not be pinned in
// browsers for a year.
type cacheStatusWriter struct {
	http.ResponseWriter
	wrote bool
}

// WriteHeader records that headers were sent and sets the immutable
// Cache-Control header for 2xx responses before delegating.
func (w *cacheStatusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if code >= 200 && code < 300 {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implicitly sends a 200 status (via WriteHeader) before writing,
// matching the normal http.ResponseWriter contract.
func (w *cacheStatusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// withCacheHeaders marks static assets as immutable; templates reference them
// with a version query param (e.g. app.css?v=1) for cache busting.
func (s *Server) withCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&cacheStatusWriter{ResponseWriter: w}, r)
	})
}

// securityHeaders sets baseline hardening headers on every response.
// Authenticated HTML, the API, and one-time secrets are no-store; the
// static file handler overrides with its own immutable policy.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		// HSTS only where the app knows it is served over TLS (directly or
		// via IDLER_BEHIND_TLS_PROXY) — on plain http the header is ignored
		// by browsers anyway, and a proxy that also sets it is harmless.
		if s.cookieSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; script-src 'self'; "+
				"base-uri 'none'; form-action 'self'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware converts panics into 500s instead of dropping the connection.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic serving request", "err", err, "path", r.URL.Path)
				http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// randomToken returns n random bytes encoded URL-safe.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
