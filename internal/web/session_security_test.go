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

package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestSessionIssuanceRejectsResetCredential reproduces the race between
// password verification and a concurrent reset: createSession must refuse to
// issue a session (and set no cookie) once the verified hash is stale, while
// a session created against the current hash still succeeds.
func TestSessionIssuanceRejectsResetCredential(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	var id int
	var oldHash string
	if err := database.QueryRow("SELECT id, password_hash FROM users").Scan(&id, &oldHash); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/login", nil)
	before := httptest.NewRecorder()
	if err := srv.createSession(before, request, id, oldHash); err != nil {
		t.Fatal(err)
	}
	// Deterministically reproduce the race: verification copied oldHash, then
	// the reset commits before the delayed session insertion resumes.
	newHash, err := bcrypt.GenerateFromPassword([]byte("replacement-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE users SET password_hash = ?, api_token_hash = NULL WHERE id = ?", string(newHash), id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE user_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	stale := httptest.NewRecorder()
	if err := srv.createSession(stale, request, id, oldHash); !errors.Is(err, errCredentialsChanged) {
		t.Fatalf("stale credential: got %v", err)
	}
	if len(stale.Result().Cookies()) != 0 {
		t.Fatal("stale credential received a cookie")
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("reset left %d sessions", count)
	}

	// The pre-reset cookie stays invalid, and the current credential can log in.
	revoked := httptest.NewRequest(http.MethodGet, "/", nil)
	revoked.AddCookie(before.Result().Cookies()[0])
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, revoked)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatalf("revoked session accepted: %d", response.Code)
	}
	current := httptest.NewRecorder()
	if err := srv.createSession(current, request, id, string(newHash)); err != nil {
		t.Fatal(err)
	}
	if len(current.Result().Cookies()) != 1 {
		t.Fatal("current credential did not receive a cookie")
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("current credential created %d sessions", count)
	}
}

// TestPasswordChangeSessionIssuanceErrors checks stale credentials and storage failures.
func TestPasswordChangeSessionIssuanceErrors(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		wantStatus    int
	}{
		{
			name: "superseded credential",
			// Supersede the new hash during session revocation to deterministically
			// exercise a reset completing before replacement session issuance.
			trigger: `CREATE TRIGGER supersede_password AFTER DELETE ON sessions BEGIN
				UPDATE users SET password_hash = 'superseded' WHERE id = OLD.user_id;
			END`,
			wantStatus: http.StatusSeeOther,
		},
		{
			name: "database failure",
			trigger: `CREATE TRIGGER fail_session BEFORE INSERT ON sessions BEGIN
				SELECT RAISE(FAIL, 'synthetic session storage failure');
			END`,
			wantStatus: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, database := newTestServer(t)
			client := authedClient(t, ts)
			if _, err := database.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			resp := postForm(t, client, ts, "/settings/account", url.Values{
				"action": {"password"}, "current_password": {testPassword},
				"new_password": {"replacement-password"}, "confirm_password": {"replacement-password"},
			})
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusSeeOther && resp.Header.Get("Location") != "/login" {
				t.Fatalf("redirect = %q, want /login", resp.Header.Get("Location"))
			}
			if hasSessionCookie(resp) {
				t.Fatal("failed issuance set a session cookie")
			}
			var count int
			if err := database.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("failed issuance created %d sessions", count)
			}
		})
	}
}
