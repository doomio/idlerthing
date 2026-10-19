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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if b.SSHHostMetrics || b.SSHMemoryMetrics || b.SSHDirectoryMetrics || b.SSHQuotaMetrics || b.SSHTrafficMetrics {
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

func TestAccountMetricOptInsAndParsing(t *testing.T) {
	box := &model.Seedbox{SSHQuotaMetrics: true, SSHTrafficMetrics: true}
	command := sshAccountCommand(box)
	if !strings.Contains(command, `"$HOME/.quota"`) || !strings.Contains(command, `"$HOME/.trafficLimit"`) {
		t.Fatalf("account command %q", command)
	}
	if sshAccountCommand(&model.Seedbox{}) != "" {
		t.Fatal("opted-out account command")
	}
	result, err := parseSSHAccountSnapshot("quota=demo 1234 2048 4096 1 2 3\ntraffic_limit=2.5\nbonus_traffic=0.5\nthrottled=0\n", box)
	if err != nil || !strings.Contains(result, "1 / 2 MiB") || !strings.Contains(result, "3 GiB") {
		t.Fatalf("account summary %q %v", result, err)
	}
	if result, err := parseSSHAccountSnapshot("throttled=0\n", &model.Seedbox{SSHTrafficMetrics: true}); err != nil || !strings.Contains(result, "unavailable") {
		t.Fatalf("missing limit: %q %v", result, err)
	}
	if result, err := parseSSHAccountSnapshot("quota=/dev/md2 1659G 2000G 2500G 12345 1000k 1250k\n", &model.Seedbox{SSHQuotaMetrics: true}); err != nil || !strings.Contains(result, "1698816 / 2048000 MiB") {
		t.Fatalf("humanized quota: %q %v", result, err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"quota=warning only\nquota_file=demo 1234 2048 4096 1 2 3\n", "1 / 2 MiB"},
		{"quota=demo 1234 0 0 1 2 3\n", "quota not set"},
		{"quota=malformed\ntraffic_limit=2.5\nthrottled=0\n", "Traffic limit: 2.5 GiB"},
	} {
		if got, err := parseSSHAccountSnapshot(tc.raw, box); err != nil || !strings.Contains(got, tc.want) {
			t.Fatalf("partial account snapshot: %q %q %v", tc.raw, got, err)
		}
	}
	for _, raw := range []string{"", strings.Repeat("x", 1025)} {
		if _, err := parseSSHAccountSnapshot(raw, box); err == nil {
			t.Fatalf("accepted %q", raw[:min(35, len(raw))])
		}
	}
}

func TestDirectoryBreakdownPersistenceFailureAndExport(t *testing.T) {
	ts, db, srv := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHDirectoryMetrics: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	privateName := "private-directory-marker"
	srv.sshPoll = func(context.Context, *model.Seedbox) (sshPollResult, error) {
		return sshPollResult{Summary: "Top-level home entries: 1", DirectoryOK: true, Directory: []directoryEntry{{Name: privateName, Size: 1048576}}}, nil
	}
	poll := func() {
		req := httptest.NewRequest(http.MethodPost, "/seedboxes/"+strconv.FormatInt(id, 10)+"/poll", nil)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		w := httptest.NewRecorder()
		srv.handleSeedboxPoll(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("poll: %d", w.Code)
		}
	}
	poll()
	var data, at string
	if err := db.QueryRow(`SELECT directory_entries, directory_at FROM ssh_observations WHERE seedbox_id = ?`, id).Scan(&data, &at); err != nil || !strings.Contains(data, privateName) || at == "" {
		t.Fatalf("stored: %q %q %v", data, at, err)
	}
	srv.sshPoll = func(context.Context, *model.Seedbox) (sshPollResult, error) {
		return sshPollResult{}, errors.New(privateName)
	}
	poll()
	var after, afterAt string
	if err := db.QueryRow(`SELECT directory_entries, directory_at FROM ssh_observations WHERE seedbox_id = ?`, id).Scan(&after, &afterAt); err != nil || after != data || afterAt != at {
		t.Fatal("failed poll discarded last directory result")
	}
	client := authedClient(t, ts)
	for _, path := range []string{"/seedboxes/" + strconv.FormatInt(id, 10), "/export/json", "/export/csv"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		contains := bytes.Contains(body, []byte(privateName))
		if strings.HasPrefix(path, "/seedboxes/") != contains {
			t.Fatalf("privacy %s: name present=%v", path, contains)
		}
	}
}

func TestDirectoryBreakdownBoundedAndPrivate(t *testing.T) {
	var raw strings.Builder
	for i := 0; i < 700; i++ {
		raw.WriteString(strconv.Itoa(i+1) + "\t./item" + strconv.Itoa(i) + "\x00")
	}
	entries, restCount, restSize, err := parseSSHDirectoryBreakdown(raw.String())
	if err != nil || len(entries) != 500 || restCount != 200 || restSize != 20100 || entries[0].Name != "item699" {
		t.Fatalf("breakdown: %d %d %d %v", len(entries), restCount, restSize, err)
	}
	if _, _, _, err := parseSSHDirectoryBreakdown("1\t./bad/name\x00"); err == nil {
		t.Fatal("accepted nested path")
	}
}

func TestDirectoryUsageSelectionAndBounds(t *testing.T) {
	box := &model.Seedbox{SSHDirectoryMetrics: true}
	if got := sshDirectoryCommand(box); !strings.Contains(got, `cd "$HOME"`) || !strings.Contains(got, "du -0 -xs -B1 --") || !strings.Contains(got, "nice -n19") {
		t.Fatalf("unsafe directory command: %q", got)
	}
	if got := sshDirectoryCommand(&model.Seedbox{}); got != "" {
		t.Fatalf("opted-out command: %q", got)
	}
	count, size, err := parseSSHDirectoryUsage("1024	./data\x002048	./.cache\x00")
	if err != nil || count != 2 || size != 3072 {
		t.Fatalf("directory summary: %d %d %v", count, size, err)
	}
	if got, size, err := parseSSHDirectoryUsage("\x00"); err != nil || got != 0 || size != 0 {
		t.Fatalf("empty home: %d %d %v", got, size, err)
	}
	for _, raw := range []string{"", "-1\t./x\x00", "1024\t/etc/passwd\x00", "1024\t./a\x00malformed\x00", "1024\t./a\x00partial", strings.Repeat("x", 8193)} {
		if _, _, err := parseSSHDirectoryUsage(raw); err == nil {
			t.Fatalf("accepted malformed directory output: %q", raw[:min(40, len(raw))])
		}
	}
}

func TestDirectoryCommandScopedToHome(t *testing.T) {
	if _, err := exec.LookPath("du"); err != nil {
		t.Skip("du unavailable")
	}
	if err := exec.Command("du", "--version").Run(); err != nil {
		t.Skip("GNU du unavailable")
	}
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "data", "file"), []byte("demo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(home, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "..hidden"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "line\nbreak"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", sshDirectoryCommand(&model.Seedbox{SSHDirectoryMetrics: true}))
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	count, _, err := parseSSHDirectoryUsage(string(out))
	if err != nil || count != 4 {
		t.Fatalf("scoped directory output: count=%d err=%v", count, err)
	}
}

func TestDirectoryPollKeepsResultWhenHostUnavailable(t *testing.T) {
	box := &model.Seedbox{SSHHostMetrics: true, SSHDirectoryMetrics: true}
	var commands []string
	poll := func(_ context.Context, _ *model.Seedbox, command string, _ time.Duration, _ int) (string, error) {
		commands = append(commands, command)
		if strings.Contains(command, "/proc/loadavg") {
			return "", errors.New("unavailable")
		}
		return "1024\t./data\x00", nil
	}
	result, err := collectSSHMetrics(t.Context(), box, poll)
	if err != nil || !strings.Contains(result, "Top-level home entries: 1") || !strings.Contains(result, "Host metrics unavailable") || len(commands) != 2 {
		t.Fatalf("partial result: %q %v commands=%d", result, err, len(commands))
	}
}

func TestSSHObservationPersistsAcrossViewAndFailure(t *testing.T) {
	_, db, srv := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHDirectoryMetrics: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.sshPoll = func(context.Context, *model.Seedbox) (sshPollResult, error) {
		return sshPollResult{Summary: "Top-level home entries: 2 (3 MB total)"}, nil
	}
	poll := func() {
		req := httptest.NewRequest(http.MethodPost, "/seedboxes/1/poll", nil)
		req.SetPathValue("id", "1")
		w := httptest.NewRecorder()
		srv.handleSeedboxPoll(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("poll status %d", w.Code)
		}
	}
	poll()
	var result, successAt, attemptedAt string
	var lastError string
	read := func() {
		t.Helper()
		if err := db.QueryRow(`SELECT result, success_at, attempted_at, last_error FROM ssh_observations WHERE seedbox_id = ?`, id).Scan(&result, &successAt, &attemptedAt, &lastError); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if result != "Top-level home entries: 2 (3 MB total)" || successAt == "" || attemptedAt == "" || lastError != "" {
		t.Fatalf("saved observation: %q %q %q %q", result, successAt, attemptedAt, lastError)
	}
	srv.sshPoll = func(context.Context, *model.Seedbox) (sshPollResult, error) {
		return sshPollResult{}, errors.New("private remote error")
	}
	poll()
	read()
	if result != "Top-level home entries: 2 (3 MB total)" || lastError == "" || strings.Contains(lastError, "private") {
		t.Fatalf("failure replaced result or leaked error: %q %q", result, lastError)
	}
	if err := (&model.SeedboxStore{DB: db}).Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM ssh_observations WHERE seedbox_id = ?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphaned observation: count=%d err=%v", count, err)
	}
}

func TestSSHObservationDetailShowsLastResult(t *testing.T) {
	ts, db, _ := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHDirectoryMetrics: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ssh_observations(seedbox_id, result, success_at, attempted_at, last_error) VALUES (?, ?, ?, ?, ?)`, id, "Top-level home entries: 2 (3 MB total)", "2026-10-18 22:00:00 UTC", "2026-10-18 23:00:00 UTC", "SSH observation unavailable"); err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/seedboxes/" + strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Top-level home entries: 2", "2026-10-18 22:00:00 UTC", "SSH observation unavailable"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("detail missing %q", want)
		}
	}
}

func TestDirectoryOnlyPollOptIn(t *testing.T) {
	_, db, srv := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHDirectoryMetrics: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	srv.sshPoll = func(_ context.Context, box *model.Seedbox) (sshPollResult, error) {
		called = true
		if box.ID != id || !box.SSHDirectoryMetrics || box.SSHHostMetrics || box.SSHMemoryMetrics {
			t.Fatalf("wrong metrics: %+v", box)
		}
		return sshPollResult{Summary: "Top-level home entries: 2"}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/seedboxes/1/poll", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	srv.handleSeedboxPoll(w, req)
	if w.Code != http.StatusSeeOther || !called {
		t.Fatalf("directory-only poll: %d called=%v", w.Code, called)
	}
}

func TestAccountQuotaInvalidRowDoesNotMaskTraffic(t *testing.T) {
	got, err := parseSSHAccountSnapshot("quota=malformed\ntraffic_limit=2.5\nthrottled=0\n", &model.Seedbox{SSHQuotaMetrics: true, SSHTrafficMetrics: true})
	if err != nil || !strings.Contains(got, "Account disk quota unavailable") || !strings.Contains(got, "Traffic limit: 2.5 GiB") {
		t.Fatalf("partial metric: %q %v", got, err)
	}
}

func TestAccountOnlyPollOptIn(t *testing.T) {
	_, db, srv := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHQuotaMetrics: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	srv.sshPoll = func(_ context.Context, box *model.Seedbox) (sshPollResult, error) {
		called = true
		if box.ID != id || !box.SSHQuotaMetrics || box.SSHTrafficMetrics || box.SSHDirectoryMetrics {
			t.Fatalf("wrong metric selection: %+v", box)
		}
		return sshPollResult{Summary: "Account disk used / soft quota: 1 / 2 MiB"}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/seedboxes/1/poll", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	srv.handleSeedboxPoll(w, req)
	if w.Code != http.StatusSeeOther || !called {
		t.Fatalf("account-only poll: %d called=%v", w.Code, called)
	}
}

func TestSSHPollExportImportRoundTrip(t *testing.T) {
	ts, db, _ := newTestServerFull(t)
	_, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", SSHHostMetrics: true, SSHMemoryMetrics: true, SSHDirectoryMetrics: true, SSHQuotaMetrics: true, SSHTrafficMetrics: true}, nil)
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
	if !bytes.Contains(body, []byte(`"ssh_poll_enabled":true`)) || !bytes.Contains(body, []byte(`"ssh_user":"demo"`)) || !bytes.Contains(body, []byte(`"ssh_host_metrics":true`)) || !bytes.Contains(body, []byte(`"ssh_memory_metrics":true`)) || !bytes.Contains(body, []byte(`"ssh_directory_metrics":true`)) || !bytes.Contains(body, []byte(`"ssh_quota_metrics":true`)) || !bytes.Contains(body, []byte(`"ssh_traffic_metrics":true`)) {
		t.Fatal("SSH opt-in absent from export")
	}
	restored := freshDB(t)
	if _, err := importer.Import(t.Context(), restored, bytes.NewReader(body), false); err != nil {
		t.Fatal(err)
	}
	b, _, err := (&model.SeedboxStore{DB: restored}).Get(t.Context(), 1)
	if err != nil || !b.SSHPollEnabled || b.SSHUser != "demo" || !b.SSHHostMetrics || !b.SSHMemoryMetrics || !b.SSHDirectoryMetrics || !b.SSHQuotaMetrics || !b.SSHTrafficMetrics {
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
	srv.sshPoll = func(_ context.Context, got *model.Seedbox) (sshPollResult, error) {
		called = true
		if got.ID != id || got.Hostname != b.Hostname || got.SSHUser != "demo" {
			t.Fatalf("wrong target: %+v", got)
		}
		return sshPollResult{Summary: "0.14 0.10 0.05 1/100 123"}, nil
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
