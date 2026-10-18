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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

// fleetFixture seeds a keyed provider, two cached services and linked
// seedboxes: one active, one cancelled, and one whose service is not cached.
func fleetFixture(t *testing.T, database *sql.DB) (providerID int64) {
	t.Helper()
	return fleetFixtureRates(t, database, nil)
}

// fleetFixtureRates additionally installs rates, so a non-USD price converts
// to a predictable monthly figure. A 1:1 rate keeps the arithmetic out of the
// way: EUR 44.97 per quarter is 14.99 per month either side of the conversion.
func fleetFixtureRates(t *testing.T, database *sql.DB, srv *Server) (providerID int64) {
	t.Helper()
	if srv != nil {
		ratesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"base":"USD","rates":{"EUR":1}}`)
		}))
		t.Cleanup(ratesSrv.Close)
		srv.rates.BaseURL = ratesSrv.URL
	}
	providerID = seedProviderWithKey(t, database, testProviderKey)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// A linked, active seedbox with pricing the sync would have written.
	res, err := tx.Exec(`INSERT INTO seedboxes (hostname, provider_id, pm_service_id, active, disk_as_mb)
		VALUES ('box1.example.net', ?, 101, 1, 1000)`, providerID)
	if err != nil {
		t.Fatal(err)
	}
	box1, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date, active)
		VALUES (?, ?, 'EUR', 44.97, 2, '2027-05-03', 1)`, box1, model.ServiceSeedbox); err != nil {
		t.Fatal(err)
	}
	res, err = tx.Exec(`INSERT INTO seedboxes (hostname, provider_id, pm_service_id, active)
		VALUES ('box2.example.net', ?, 102, 1)`, providerID)
	if err != nil {
		t.Fatal(err)
	}
	// A linked seedbox whose service is absent from the cache below: nothing
	// to render, so it must not appear and must not inflate the tiles.
	if _, err := tx.Exec(`INSERT INTO seedboxes (hostname, provider_id, pm_service_id, active)
		VALUES ('ghost.example.net', ?, 909, 1)`, providerID); err != nil {
		t.Fatal(err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := (&model.PMStore{DB: database}).Replace(t.Context(), providerID, []model.PMService{
		{
			ProviderID: providerID, ServiceID: 101, Product: "Dragon-R", Status: "Active",
			NextDue: "2027-05-03", BillingCycle: "Quarterly", RecurringAmount: 44.97, Currency: "EUR",
			DiskUsedBytes: 307260497920, DiskQuotaBytes: 9910637035520,
			SentBytes: 2905931361157, ReceivedBytes: 1915497397, CPUHours: 1, Torrents: 7,
			SyncedAt: "2026-10-18T00:00:00Z",
		},
		{
			ProviderID: providerID, ServiceID: 102, Product: "Old Box", Status: "Cancelled",
			RecurringAmount: 99, Currency: "EUR", SyncedAt: "2026-10-18T00:00:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	return providerID
}

func TestFleetRequiresLogin(t *testing.T) {
	ts, database := newTestServer(t)
	fleetFixture(t, database)
	client := newClient(t)

	resp, err := client.Get(ts.URL + "/seedboxes/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated fleet access must redirect to login, got %d", resp.StatusCode)
	}
}

func TestFleetHidesCancelledByDefault(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	fleetFixtureRates(t, database, srv)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/seedboxes/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fleet view: %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Dragon-R") {
		t.Fatal("an active service must be listed")
	}
	if strings.Contains(body, "Old Box") {
		t.Fatal("a cancelled service must be hidden by default")
	}
	// A link with no cached service has nothing to render.
	if strings.Contains(body, "ghost.example.net") {
		t.Fatal("a box whose service is not cached must not be listed")
	}
}

func TestFleetShowsCancelledWithParam(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	fleetFixtureRates(t, database, srv)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/seedboxes/fleet?cancelled=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Dragon-R") {
		t.Fatal("active service missing")
	}
	if !strings.Contains(body, "Old Box") {
		t.Fatal("?cancelled=1 must reveal the cancelled service")
	}
}

// Cancelled services must not contribute to the headline figures in either
// state: a cancelled box's price is not money being spent.
func TestFleetExcludesCancelledFromTotals(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	fleetFixtureRates(t, database, srv)
	client := authedClient(t, ts)

	// The cancelled service carries 99 EUR annually = 8.25/month, which must
	// never appear. The active one is 44.97 quarterly = 14.99/month.
	for _, q := range []string{"", "?cancelled=1"} {
		resp, err := client.Get(ts.URL + "/seedboxes/fleet" + q)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("fleet view %q: status %d", q, resp.StatusCode)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if strings.Contains(body, "8.25") {
			t.Fatalf("a cancelled service's cost leaked into the totals (%q)", q)
		}
		// The active service's own monthly cost must be present, so this test
		// cannot pass by rendering an empty page.
		if !strings.Contains(body, "14.99") {
			t.Fatalf("the active service's monthly cost is missing (%q)", q)
		}
	}
}

// Feed data is unauthenticated-route-free: /public renders servers only, and a
// linked seedbox must not surface there.
func TestFleetExcludesPMBoxesFromPublic(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	fleetFixtureRates(t, database, srv)
	// Turn on the public page and publish the PM-linked seedbox.
	if _, err := database.Exec("UPDATE settings SET servers_public = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE seedboxes SET show_public = 1"); err != nil {
		t.Fatal(err)
	}

	resp, err := newClient(t).Get(ts.URL + "/public")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/public: %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, secret := range []string{"box1.example.net", "Dragon-R", "44.97"} {
		if strings.Contains(body, secret) {
			t.Fatalf("/public leaked PM-backed seedbox data (%q)", secret)
		}
	}
}

// A provider with no key has no feed: the fleet view is empty, not an error.
func TestFleetEmptyWithoutKey(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/seedboxes/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fleet view must render with no feed configured, got %d", resp.StatusCode)
	}
}

// The sync control is a same-origin POST: it must be refused without a valid
// CSRF token, like every other mutating route.
func TestFleetSyncRequiresCSRF(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	fleetFixtureRates(t, database, srv)
	client := authedClient(t, ts)

	resp, err := client.PostForm(ts.URL+"/seedboxes/fleet/sync", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The route must exist and reject the request, not merely 404.
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("the sync route does not exist")
	}
	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusOK {
		t.Fatalf("a CSRF-less sync must be rejected, got %d", resp.StatusCode)
	}
}
