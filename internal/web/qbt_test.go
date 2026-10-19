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

// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
// SPDX-License-Identifier: AGPL-3.0-or-later
package web

import (
	"bytes"
	"context"
	"errors"
	"idlerthing/internal/importer"
	"idlerthing/internal/model"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQBTObservation(t *testing.T) {
	b := &model.Seedbox{Active: true, SSHPollEnabled: true, SSHUser: "demo", QBTEnabled: true}
	good := `{"server_state":{"dl_info_speed":1024,"up_info_speed":2048},"torrents":{"a":{"state":"downloading"},"b":{"state":"seeding"}}}`
	result, err := collectQBT(t.Context(), b, func(_ context.Context, _ *model.Seedbox, cmd string, _ time.Duration, cap int) (string, error) {
		if !strings.Contains(cmd, "/api/v2/sync/maindata?rid=0") || !strings.Contains(cmd, "127.0.0.1") || cap > 16<<20+8192 {
			t.Fatalf("unsafe command or cap: %d %s", cap, cmd)
		}
		return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + good, nil
	})
	if err != nil || !strings.Contains(result, "2 torrents") || !strings.Contains(result, "1 KiB/s") {
		t.Fatalf("%s %v", result, err)
	}
	for _, raw := range []string{`{}`, `{"torrents":[]}`, `{"torrents":{},"server_state":{}}`, "HTTP/1.1 403 Forbidden\r\n\r\nauth required", "HTTP/1.1 302 Found\r\nLocation: http://example.org/\r\n\r\n"} {
		if !strings.HasPrefix(raw, "HTTP/") {
			raw = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + raw
		}
		if _, err := parseQBT(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, tc := range []struct{ enabled, active, ssh bool }{{false, true, true}, {true, false, true}, {true, true, false}} {
		b.QBTEnabled, b.Active, b.SSHPollEnabled = tc.enabled, tc.active, tc.ssh
		called := false
		_, err := collectQBT(t.Context(), b, func(context.Context, *model.Seedbox, string, time.Duration, int) (string, error) {
			called = true
			return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + good, nil
		})
		if called || err == nil {
			t.Fatalf("unsafe gate %+v", tc)
		}
	}
}

func TestQBTDetailAndExport(t *testing.T) {
	ts, db, _ := newTestServerFull(t)
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo", QBTEnabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO qbt_observations(seedbox_id,result,success_at,attempted_at,last_error) VALUES(?,?,?,?,?)`, id, "2 torrents; down 1 KiB/s", "2026-10-19 10:00:00 UTC", "2026-10-19 11:00:00 UTC", "qBittorrent observation unavailable"); err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/seedboxes/" + strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Observe qBittorrent", "2 torrents; down 1 KiB/s", "2026-10-19 10:00:00 UTC", "qBittorrent observation unavailable"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("missing %q", want)
		}
	}
	resp, err = client.Get(ts.URL + "/export/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"qbt_enabled":true`)) {
		t.Fatalf("export missing opt-in: %s", body)
	}
	restored := freshDB(t)
	if _, err := importer.Import(t.Context(), restored, bytes.NewReader(body), false); err != nil {
		t.Fatal(err)
	}
	box, _, err := (&model.SeedboxStore{DB: restored}).Get(t.Context(), 1)
	if err != nil || !box.QBTEnabled {
		t.Fatalf("import lost opt-in: %+v %v", box, err)
	}
}

func TestQBTBoundedResponse(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\n\r\n" + strings.Repeat("x", 16<<20+1)
	if _, err := parseQBT(raw); err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestQBTHandlerGateAndPersistence(t *testing.T) {
	_, db, s := newTestServerFull(t)
	b := &model.Seedbox{Hostname: "box.example.net", Active: true, SSHPollEnabled: true, SSHUser: "demo"}
	id, err := (&model.SeedboxStore{DB: db}).Create(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.ID = id
	calls := 0
	s.qbtPoll = func(context.Context, *model.Seedbox) (string, error) { calls++; return "2 torrents; down 1 KiB/s", nil }
	request := func() int {
		r := httptest.NewRequest(http.MethodPost, "/seedboxes/"+strconv.FormatInt(id, 10)+"/poll-qbt", nil)
		r.SetPathValue("id", strconv.FormatInt(id, 10))
		w := httptest.NewRecorder()
		s.handleQBTPoll(w, r)
		return w.Code
	}
	if code := request(); code != http.StatusForbidden || calls != 0 {
		t.Fatalf("disabled: %d %d", code, calls)
	}
	b.QBTEnabled = true
	if err := (&model.SeedboxStore{DB: db}).Update(t.Context(), b, nil); err != nil {
		t.Fatal(err)
	}
	if code := request(); code != http.StatusSeeOther || calls != 1 {
		t.Fatalf("poll: %d %d", code, calls)
	}
	var result, success, attempted, lastErr string
	read := func() {
		t.Helper()
		if err := db.QueryRow(`SELECT result, success_at, attempted_at, last_error FROM qbt_observations WHERE seedbox_id=?`, id).Scan(&result, &success, &attempted, &lastErr); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if result != "2 torrents; down 1 KiB/s" || success == "" || attempted == "" || lastErr != "" {
		t.Fatalf("state: %q %q %q %q", result, success, attempted, lastErr)
	}
	s.qbtPoll = func(context.Context, *model.Seedbox) (string, error) { return "", errors.New("secret remote error") }
	if code := request(); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	read()
	if result == "" || success == "" || lastErr == "" || strings.Contains(lastErr, "secret") {
		t.Fatalf("error state: %q %q %q", result, success, lastErr)
	}
	b.Active = false
	if err := (&model.SeedboxStore{DB: db}).Update(t.Context(), b, nil); err != nil {
		t.Fatal(err)
	}
	if code := request(); code != http.StatusForbidden {
		t.Fatal(code)
	}
}
