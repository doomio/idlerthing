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
	"database/sql"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// seedProviderService creates a provider with a key and one cached feed
// service, returning both ids.
func seedProviderService(t *testing.T, database *sql.DB) (providerID, serviceID int64) {
	t.Helper()
	providerID = seedProviderWithKey(t, database, testProviderKey)
	if err := (&model.PMStore{DB: database}).Replace(t.Context(), providerID, []model.PMService{
		{ProviderID: providerID, ServiceID: 555, Product: "Dragon-R", SyncedAt: "2026-10-18T00:00:00Z"},
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	return providerID, 555
}

func seedSeedboxRow(t *testing.T, database *sql.DB, providerID int64, hostname string) int64 {
	t.Helper()
	res, err := database.Exec("INSERT INTO seedboxes (hostname, provider_id, active) VALUES (?, ?, 1)",
		hostname, providerID)
	if err != nil {
		t.Fatalf("seed seedbox: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func readSeedboxLink(t *testing.T, database *sql.DB, id int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := database.QueryRow("SELECT pm_service_id FROM seedboxes WHERE id = ?", id).Scan(&v); err != nil {
		t.Fatalf("read link: %v", err)
	}
	return v
}

// The seedbox form offers the provider's feed services, so a link can be made
// (or cleared) by hand.
func TestSeedboxFormOffersFeedServices(t *testing.T) {
	ts, database := newTestServer(t)
	providerID, _ := seedProviderService(t, database)
	box := seedSeedboxRow(t, database, providerID, "box1.example.net")
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/seedboxes/" + itoa64(box) + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "pm_service_id") {
		t.Fatal("the form must offer a feed-service field")
	}
	if !strings.Contains(body, "Dragon-R") {
		t.Fatal("the form must list the provider's feed services")
	}
}

// A provider without a key has no feed, so the control is not shown.
func TestSeedboxFormHidesFeedServicesWithoutKey(t *testing.T) {
	ts, database := newTestServer(t)
	st := &model.CatalogStore{DB: database}
	id, err := st.Create(t.Context(), model.Catalogs["providers"], "No Key Host")
	if err != nil {
		t.Fatal(err)
	}
	box := seedSeedboxRow(t, database, id, "box1.example.net")
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/seedboxes/" + itoa64(box) + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if strings.Contains(readBody(t, resp), "pm_service_id") {
		t.Fatal("a provider with no key must not show a feed link control")
	}
}

// The link round-trips through the form.
func TestSeedboxFormSavesLink(t *testing.T) {
	ts, database := newTestServer(t)
	providerID, serviceID := seedProviderService(t, database)
	box := seedSeedboxRow(t, database, providerID, "box1.example.net")
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, "/seedboxes/"+itoa64(box)+"/update", url.Values{
		"hostname":      {"box1.example.net"},
		"active":        {"on"},
		"provider_id":   {itoa64(providerID)},
		"pm_service_id": {itoa64(serviceID)},
	})
	resp.Body.Close()

	if got := readSeedboxLink(t, database, box); !got.Valid || got.Int64 != serviceID {
		t.Fatalf("link not saved: %+v", got)
	}
}

// Clearing the select unlinks.
func TestSeedboxFormClearsLink(t *testing.T) {
	ts, database := newTestServer(t)
	providerID, _ := seedProviderService(t, database)
	box := seedSeedboxRow(t, database, providerID, "box1.example.net")
	if _, err := database.Exec("UPDATE seedboxes SET pm_service_id = 555 WHERE id = ?", box); err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, "/seedboxes/"+itoa64(box)+"/update", url.Values{
		"hostname":      {"box1.example.net"},
		"active":        {"on"},
		"provider_id":   {itoa64(providerID)},
		"pm_service_id": {""},
	})
	resp.Body.Close()

	if got := readSeedboxLink(t, database, box); got.Valid {
		t.Fatalf("empty select must unlink, got %+v", got)
	}
}

// A POST that never carried the field (a client that predates it, or a
// partial update) must not silently drop the user's link.
func TestSeedboxUpdatePreservesLinkWhenFieldAbsent(t *testing.T) {
	ts, database := newTestServer(t)
	providerID, serviceID := seedProviderService(t, database)
	box := seedSeedboxRow(t, database, providerID, "box1.example.net")
	if _, err := database.Exec("UPDATE seedboxes SET pm_service_id = ? WHERE id = ?", serviceID, box); err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)

	// No pm_service_id key at all.
	resp := postForm(t, client, ts, "/seedboxes/"+itoa64(box)+"/update", url.Values{
		"hostname":    {"box1.example.net"},
		"active":      {"on"},
		"provider_id": {itoa64(providerID)},
	})
	resp.Body.Close()

	if got := readSeedboxLink(t, database, box); !got.Valid || got.Int64 != serviceID {
		t.Fatalf("an absent field must preserve the link, got %+v", got)
	}
}

// A link pointing at a service the provider's feed no longer returns is not
// shown as valid, but must not be destroyed by an unrelated save.
func TestSeedboxFormKeepsStaleLink(t *testing.T) {
	ts, database := newTestServer(t)
	providerID, _ := seedProviderService(t, database)
	box := seedSeedboxRow(t, database, providerID, "box1.example.net")
	// 999 is not in the cache.
	if _, err := database.Exec("UPDATE seedboxes SET pm_service_id = 999 WHERE id = ?", box); err != nil {
		t.Fatal(err)
	}
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/seedboxes/" + itoa64(box) + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "999") {
		t.Fatal("a stale link must still be shown, so saving cannot silently drop it")
	}
}
