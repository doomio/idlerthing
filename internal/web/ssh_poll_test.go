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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

func TestSSHPollMetricsFlagsDefaultOff(t *testing.T) {
	_, db, _ := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := (&model.SeedboxStore{DB: db}).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if b.SSHHostMetrics || b.SSHMemoryMetrics {
		t.Fatalf("metrics enabled without opt-in: %+v", b)
	}
}

func TestParseSSHHostSnapshot(t *testing.T) {
	snap, err := parseSSHHostSnapshot("0.14 0.10 0.05 1/100 123\nMemTotal: 4096 kB\nMemAvailable: 1024 kB\n")
	if err != nil || snap.Load != "0.14 / 0.10 / 0.05" || snap.MemoryUsed != 3072*1024 || snap.MemoryTotal != 4096*1024 {
		t.Fatalf("snapshot: %+v err=%v", snap, err)
	}
	for _, bad := range []string{"NaN 0 0\nMemTotal: 1 kB\nMemAvailable: 1 kB", "0.1 0.2", "0.1 0.2 0.3\nMemTotal: 1 kB\nMemAvailable: 2 kB", strings.Repeat("x", 257)} {
		if _, err := parseSSHHostSnapshot(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSSHMetricSelection(t *testing.T) {
	load := "0.14 0.10 0.05 1/100 123\n"
	memory := "MemTotal: 4096 kB\nMemAvailable: 1024 kB\n"
	for _, tc := range []struct {
		name                string
		host, mem           bool
		output, wantCommand string
	}{
		{"load only", true, false, load, "cat /proc/loadavg"},
		{"memory only", false, true, memory, "grep -E '^(MemTotal|MemAvailable):' /proc/meminfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &model.Seedbox{SSHHostMetrics: tc.host, SSHMemoryMetrics: tc.mem}
			if got := sshMetricsCommand(box); got != tc.wantCommand {
				t.Fatalf("command %q, want %q", got, tc.wantCommand)
			}
			if _, err := parseSelectedSSHMetrics(tc.output, box); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := parseSelectedSSHMetrics("MemTotal: 4096 kB junk\nMemAvailable: 1024 kB\n", &model.Seedbox{SSHMemoryMetrics: true}); err == nil {
		t.Fatal("accepted malformed memory suffix")
	}
}

func TestSSHPollExportImportRoundTrip(t *testing.T) {
	ts, db, _ := newTestServerFull(t)
	_, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHHostMetrics: true, SSHMemoryMetrics: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/export/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"ssh_poll_enabled":true`)) || !bytes.Contains(body, []byte(`"ssh_user":"demo"`)) || !bytes.Contains(body, []byte(`"ssh_host_metrics":true`)) || !bytes.Contains(body, []byte(`"ssh_memory_metrics":true`)) {
		t.Fatal("SSH opt-in absent from export")
	}
	restored := freshDB(t)
	if _, err := importer.Import(t.Context(), restored, bytes.NewReader(body), false); err != nil {
		t.Fatal(err)
	}
	b, _, err := (&model.SeedboxStore{DB: restored}).Get(t.Context(), 1)
	if err != nil || !b.SSHPollEnabled || b.SSHUser != "demo" || !b.SSHHostMetrics || !b.SSHMemoryMetrics {
		t.Fatalf("SSH opt-in lost on restore: %+v %v", b, err)
	}
}

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
	b := &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHHostMetrics: true, SSHUser: "demo"}
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
	if !strings.Contains(w.Header().Get("Set-Cookie"), "UTC") {
		t.Fatalf("successful poll has no UTC attempt timestamp: %q", w.Header().Get("Set-Cookie"))
	}
}

func TestSSHPollRouteRequiresCSRF(t *testing.T) {
	ts, db := newTestServer(t)
	b := &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHHostMetrics: true, SSHUser: "demo"}
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

func TestSSHPollOutputBound(t *testing.T) {
	var out strings.Builder
	w := &limitedWriter{writer: &out, remaining: 8}
	if _, err := w.Write([]byte("0.1 0.2")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("x", 1024))); err == nil {
		t.Fatal("unbounded SSH response")
	}
	if out.String() != "0.1 0.2" {
		t.Fatal("overflow altered output")
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
