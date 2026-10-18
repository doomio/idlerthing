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
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

func TestOptionalSSHPollDefaultsOff(t *testing.T) {
	_, db, srv := newTestServerFull(t)
	b := &model.Seedbox{Hostname: "box.example.net", Active: true}
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err = (&model.SeedboxStore{DB: db}).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if b.SSHPollEnabled || b.SSHUser != "" {
		t.Fatalf("unexpected opt-in: %+v", b)
	}
	req := httptest.NewRequest(http.MethodPost, "/seedboxes/1/poll", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	srv.handleSeedboxPoll(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled poll: %d", w.Code)
	}
}

func TestOptionalSSHPollEnabledUsesPinnedHost(t *testing.T) {
	_, db, srv := newTestServerFull(t)
	b := &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo"}
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err = (&model.SeedboxStore{DB: db}).Get(t.Context(), id)
	if err != nil || !b.SSHPollEnabled || b.SSHUser != "demo" {
		t.Fatalf("stored opt-in: %+v %v", b, err)
	}
	called := false
	srv.sshPoll = func(_ context.Context, got *model.Seedbox) (string, error) {
		called = true
		if got.ID != id || got.Hostname != b.Hostname || got.SSHUser != "demo" {
			t.Fatalf("wrong target: %+v", got)
		}
		return "0.14 0.10 0.05 1/100 123", nil
	}
	req := httptest.NewRequest(http.MethodPost, "/seedboxes/1/poll", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	srv.handleSeedboxPoll(w, req)
	if w.Code != http.StatusSeeOther || !called {
		t.Fatalf("enabled poll: %d called=%v", w.Code, called)
	}
}

func TestSSHPollRouteRequiresCSRF(t *testing.T) {
	ts, db := newTestServer(t)
	b := &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo"}
	if _, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), b, nil); err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)
	resp, err := client.PostForm(ts.URL+"/seedboxes/1/poll", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("CSRF-less poll allowed: %d", resp.StatusCode)
	}
}

func TestSSHPollRejectsLocalTargets(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "box.local", "box.example.net -oProxyCommand", "-bad.example.net"} {
		if validSSHTarget(host) {
			t.Fatalf("accepted target %q", host)
		}
	}
	if !validSSHTarget("box.example.net") {
		t.Fatal("rejected valid DNS target")
	}
}

func TestSSHPollFormValidatesUsername(t *testing.T) {
	for _, user := range []string{"", "demo;id", "-oProxyCommand"} {
		req := httptest.NewRequest(http.MethodPost, "/seedboxes", strings.NewReader(url.Values{"hostname": {"box.example.net"}, "ssh_poll_enabled": {"on"}, "ssh_user": {user}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_, _, errs := parseSeedboxForm(req)
		if errs["ssh_user"] == "" {
			t.Fatalf("accepted username %q", user)
		}
	}
}
