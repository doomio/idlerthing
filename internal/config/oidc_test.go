package config

import "testing"

func TestOIDCConfigRequiresExplicitIdentity(t *testing.T) {
	t.Setenv("IDLER_OIDC_ISSUER", "https://auth.example/application/o/idler/")
	t.Setenv("IDLER_OIDC_CLIENT_ID", "idler")
	t.Setenv("IDLER_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("IDLER_BASE_URL", "https://idler.example")
	t.Setenv("IDLER_BEHIND_TLS_PROXY", "true")
	t.Setenv("IDLER_OIDC_SUBJECTS", "")
	cfg := Load()
	if err := cfg.ValidateOIDC(); err == nil {
		t.Fatal("OIDC must not auto-link any IdP subject to the admin account")
	}
	t.Setenv("IDLER_OIDC_SUBJECTS", "fixed-subject")
	if err := Load().ValidateOIDC(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IDLER_BASE_URL", "https://idler.example/path")
	if err := Load().ValidateOIDC(); err == nil {
		t.Fatal("base URL must be an origin")
	}
	t.Setenv("IDLER_BASE_URL", "https://idler.example")
	t.Setenv("IDLER_BEHIND_TLS_PROXY", "false")
	if err := Load().ValidateOIDC(); err == nil {
		t.Fatal("SSO cookies must be Secure behind TLS proxy")
	}
}
