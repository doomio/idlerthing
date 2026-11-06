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

// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config holds the application configuration.
type Config struct {
	// Addr is the listen address for the HTTP server.
	Addr string
	// DBPath is the path to the SQLite database file.
	DBPath string
	// AdminPassword is used only on first-run seeding of the admin user.
	AdminPassword string
	// Secret signs yabs ingest URLs (HMAC). When empty, a random secret is
	// generated and persisted next to the DB on first boot.
	Secret string
	// BehindTLSProxy marks deployments behind an HTTPS-terminating reverse
	// proxy: session cookies go Secure and X-Forwarded-For is trusted.
	BehindTLSProxy bool
	// BaseURL overrides the external URL shown in the YABS ingest command
	// (e.g. "https://idlers.example.com"). Empty = derive from requests.
	BaseURL string
	// AllowHTTPIngest permits plain-http YABS ingest URLs on LAN hosts
	// (RFC1918/link-local/ULA). Loopback http and https always work.
	AllowHTTPIngest bool
	// PMAPIURL overrides the PulsedMedia customer-API endpoint. Empty uses
	// the built-in feed URL; set only to point at a test server.
	PMAPIURL string
	// OIDC is opt-in and binds one exact IdP subject to the existing admin.
	OIDCIssuer, OIDCClientID, OIDCClientSecret, OIDCSubject string
}

// ValidateOIDC rejects partial or unsafe OIDC configuration.
func (c Config) ValidateOIDC() error {
	if c.OIDCIssuer == "" && c.OIDCClientID == "" && c.OIDCClientSecret == "" && c.OIDCSubject == "" {
		return nil
	}
	if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.OIDCSubject == "" || c.BaseURL == "" || !c.BehindTLSProxy {
		return fmt.Errorf("OIDC requires HTTPS issuer and base URL, client ID, client secret, and pinned subject")
	}
	issuer, err := url.Parse(c.OIDCIssuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.Fragment != "" || issuer.RawQuery != "" {
		return fmt.Errorf("OIDC issuer must be an HTTPS URL without userinfo or fragment")
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" {
		return fmt.Errorf("OIDC base URL must be an HTTPS origin")
	}
	return nil
}

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	return Config{
		Addr:             getEnv("IDLER_ADDR", "127.0.0.1:8080"),
		DBPath:           getEnv("IDLER_DB", "./data/idlerthing.db"),
		AdminPassword:    os.Getenv("IDLER_ADMIN_PASSWORD"),
		Secret:           os.Getenv("IDLER_SECRET"),
		BehindTLSProxy:   envBool("IDLER_BEHIND_TLS_PROXY"),
		BaseURL:          os.Getenv("IDLER_BASE_URL"),
		AllowHTTPIngest:  envBool("IDLER_ALLOW_HTTP_INGEST"),
		PMAPIURL:         os.Getenv("IDLER_PMAPI_URL"),
		OIDCIssuer:       os.Getenv("IDLER_OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("IDLER_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("IDLER_OIDC_CLIENT_SECRET"),
		OIDCSubject:      os.Getenv("IDLER_OIDC_SUBJECT"),
	}
}

// envBool reports whether an env var is set to a truthy value.
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// getEnv returns the env var key's value, or fallback when it is unset or empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
