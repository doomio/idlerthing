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

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/config"
	"idlerthing/internal/db"
)

// Batch I #9 — a short IDLER_ADMIN_PASSWORD is refused (no weak admin seeded).
func TestSeedAdminShortPassword(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := seedAdmin(database, "short", dbPath); err == nil {
		t.Fatal("expected refusal of a <8 char password")
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("no user must be seeded on refusal, got %d", count)
	}

	// A compliant password seeds; a second call is a no-op.
	if _, err := seedAdmin(database, "long-enough-password", dbPath); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := seedAdmin(database, "another-password", dbPath); err != nil {
		t.Fatalf("second seed should be a no-op: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 user, got %d", count)
	}
	// An operator-supplied password never needs a file.
	if _, err := os.Stat(dbPath + ".admin-password"); !os.IsNotExist(err) {
		t.Fatal("no password file expected when the password was supplied")
	}
}

// The generated first-run password must reach the operator as a 0600 file, not
// as an output line: under systemd stderr is the journal, which retains,
// group-reads, and forwards the credential far longer than it is needed.
func TestSeedAdminWritesBootstrapPasswordFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	path, err := seedAdmin(database, "", dbPath)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if want := dbPath + ".admin-password"; path != want {
		t.Fatalf("expected %s, got %q", want, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("password file must be 0600, got %v", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pw := strings.TrimSpace(string(raw))

	// What is in the file is what the seeded admin actually accepts.
	var hash string
	if err := database.QueryRow(
		"SELECT password_hash FROM users WHERE email = 'admin@localhost'").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		t.Fatal("the file's password does not match the seeded admin")
	}

	// With a user already present there is nothing left to seed or hand back.
	if p2, err := seedAdmin(database, "", dbPath); err != nil || p2 != "" {
		t.Fatalf("second seed should be a no-op: %q %v", p2, err)
	}
}

// A password file left over from an earlier run must never be truncated over by
// a new one — the operator deletes it deliberately to regenerate.
func TestWriteBootstrapPasswordRefusesExistingFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	path := dbPath + ".admin-password"
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBootstrapPassword(dbPath, "replacement"); err == nil {
		t.Fatal("expected refusal over an existing password file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "original\n" {
		t.Fatal("the refusal must not touch the file that was already there")
	}
}

// Batch J #5 — secret file hardening.
func TestLoadSecret(t *testing.T) {
	dir := t.TempDir()
	cfg := func() (config.Config, string) {
		dbPath := filepath.Join(dir, "test.db")
		return config.Config{DBPath: dbPath}, dbPath + ".secret"
	}

	// Short IDLER_SECRET → fatal.
	c, _ := cfg()
	c.Secret = "tooshort"
	if _, err := loadSecret(c); err == nil {
		t.Fatal("short IDLER_SECRET must be refused")
	}
	c.Secret = "0123456789abcdef"
	if s, err := loadSecret(c); err != nil || len(s) != 16 {
		t.Fatalf("valid IDLER_SECRET: %v %v", s, err)
	}

	// A loose 0644 pre-existing secret is tightened to 0600.
	c, path := cfg()
	if err := os.WriteFile(path, []byte("existing-secret-value"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecret(c); err != nil {
		t.Fatalf("load existing: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("loose secret not tightened: %o", fi.Mode().Perm())
	}

	// A symlinked secret path is fatal (never followed).
	link := filepath.Join(dir, "link.db.secret")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	c2 := config.Config{DBPath: filepath.Join(dir, "link.db")}
	if _, err := loadSecret(c2); err == nil {
		t.Fatal("symlink secret must be fatal")
	}

	// Fresh generation: 0600 from the start, second call reads it back.
	dir2 := t.TempDir()
	c3 := config.Config{DBPath: filepath.Join(dir2, "gen.db")}
	s1, err := loadSecret(c3)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if fi, _ := os.Stat(c3.DBPath + ".secret"); fi.Mode().Perm() != 0o600 {
		t.Fatalf("generated secret mode: %o", fi.Mode().Perm())
	}
	s2, err := loadSecret(c3)
	if err != nil || string(s1) != string(s2) {
		t.Fatalf("generated secret not persisted: %v", err)
	}
}

// Batch P #4 — an existing secret file must also meet the ≥16-byte rule.
func TestLoadSecretShortExistingFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{DBPath: filepath.Join(dir, "test.db")}
	if err := os.WriteFile(cfg.DBPath+".secret", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecret(cfg); err == nil {
		t.Fatal("1-byte secret file must be fatal")
	}
	// Delete → fresh generation works (and is 32 bytes).
	os.Remove(cfg.DBPath + ".secret")
	s, err := loadSecret(cfg)
	if err != nil || len(s) != 32 {
		t.Fatalf("regeneration: %v %v", s, err)
	}
}

// Batch P #2 — denylisted example passwords trigger a loud startup warning.
func TestWarnDenylistedPassword(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if _, err := seedAdmin(database, "changeme", dbPath); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil))) })
	warnDenylistedPassword(database)
	if !strings.Contains(buf.String(), "publicly documented example") {
		t.Fatalf("expected the denylist warning, got %q", buf.String())
	}

	// A non-denylisted password stays quiet.
	buf.Reset()
	if _, err := seedAdmin(database, "changeme-not-this-one", dbPath); err != nil {
		t.Fatal(err) // no-op: user exists
	}
	warnDenylistedPassword(database)
	if !strings.Contains(buf.String(), "publicly documented example") {
		t.Fatal("second denylist entry should also warn")
	}
}

// Batch P #2 — `idlerthing passwd` resets the password and revokes sessions.
func TestRunPasswd(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	t.Setenv("IDLER_DB", dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if _, err := seedAdmin(database, "old-password-1", dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO sessions (token, user_id, csrf_token, expires_at) VALUES ('t', 1, 'c', '2999-01-01')"); err != nil {
		t.Fatal(err)
	}
	database.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("new-password-2\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	if err := runPasswd(nil); err != nil {
		t.Fatalf("runPasswd: %v", err)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var hash string
	if err := database.QueryRow("SELECT password_hash FROM users").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("new-password-2")) != nil {
		t.Fatal("new password should verify")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("old-password-1")) == nil {
		t.Fatal("old password must fail after passwd")
	}
	var sessions int
	database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions)
	if sessions != 0 {
		t.Fatal("passwd must revoke all sessions")
	}
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("short\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	if err := runPasswd(nil); err == nil {
		t.Fatal("short password must be refused")
	}
}

// Batch Q N5 — passwd reads piped stdin; bad invocation shows usage.
func TestRunPasswdStdin(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	t.Setenv("IDLER_DB", dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if _, err := seedAdmin(database, "old-password-1", dbPath); err != nil {
		t.Fatal(err)
	}
	database.Close()

	// Pipe the password via stdin (no argv).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("stdin-password-9\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	if err := runPasswd(nil); err != nil {
		t.Fatalf("runPasswd(stdin): %v", err)
	}
	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var hash string
	database.QueryRow("SELECT password_hash FROM users").Scan(&hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("stdin-password-9")) != nil {
		t.Fatal("stdin password should verify")
	}

	// Any argument is rejected so plaintext credentials cannot leak through
	// process listings or shell history.
	if err := runPasswd([]string{"password-in-argv"}); err == nil || !strings.Contains(err.Error(), "usage: idlerthing passwd") {
		t.Fatalf("expected usage error, got %v", err)
	}
}
