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

package pulsedmedia

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSyncAutoAddAndManualOverride(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "auto-key")
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := svcJSON(101, "Monthly", "10", "2027-01-01", 1024)
		second := svcJSON(102, "Monthly", "20", "2027-01-01", 1024)
		second = strings.Replace(second, `"status":"Active"`, `"status":" active ","domain":"new.example.net"`, 1)
		fmt.Fprint(w, feedJSON(first, second))
	}))
	defer feed.Close()
	locked := seedLinkedSeedbox(t, database, provider, 101, "locked.example.net")
	if _, err := database.Exec("UPDATE seedboxes SET pm_manual_override = 1 WHERE id = ?", locked); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), database, provider, "auto-key", feed.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRow("SELECT count(*) FROM seedboxes WHERE provider_id = ?", provider).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("default auto-add: got %d boxes, want 2", count)
	}
	if _, _, _, _, ok := readPricing(t, database, locked); ok {
		t.Fatal("locked seedbox pricing overwritten")
	}
	var added int64
	if err := database.QueryRow("SELECT id FROM seedboxes WHERE provider_id = ? AND pm_service_id = 102", provider).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := readPricing(t, database, added); !ok {
		t.Fatal("auto-added seedbox not reconciled")
	}
	if _, err := database.Exec("UPDATE providers SET pm_auto_add = 0 WHERE id = ?", provider); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("DELETE FROM seedboxes WHERE id = ?", added); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), database, provider, "auto-key", feed.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT count(*) FROM seedboxes WHERE provider_id = ?", provider).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("disabled auto-add: got %d boxes, want 1", count)
	}
}
