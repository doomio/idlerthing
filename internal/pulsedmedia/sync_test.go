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
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/db"
	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
)

func syncDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

// feedJSON builds a feed body from raw service objects.
func feedJSON(services ...string) string {
	return `{"client_id":1,"generated_at":"2026-10-18T00:00:00Z","currency":"EUR","services":[` +
		strings.Join(services, ",") + `]}`
}

// svcJSON renders one feed service.
func svcJSON(id int64, cycle, amount, nextDue string, quotaBytes int64) string {
	return fmt.Sprintf(`{"id":%d,"product":"P%d","status":"Active","next_due":%q,
		"billing_cycle":%q,"recurring_amount":%s,
		"seedbox":{"username":"u%d","usage":{"disk_quota_bytes":%d,"disk_used_bytes":1,"torrents":3}}}`,
		id, id, nextDue, cycle, amount, id, quotaBytes)
}

// mockFeed serves one feed body.
func mockFeed(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedProviderKey creates a provider row holding key and returns its id.
func seedProviderKey(t *testing.T, database *sql.DB, key string) int64 {
	t.Helper()
	// Provider names are unique, so each call gets its own.
	name := "Example Host " + key
	res, err := database.Exec("INSERT INTO providers (name, pm_api_key) VALUES (?, ?)", name, key)
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seedLinkedSeedbox creates a seedbox already linked to a feed service.
func seedLinkedSeedbox(t *testing.T, database *sql.DB, providerID, serviceID int64, hostname string) int64 {
	t.Helper()
	res, err := database.Exec(
		"INSERT INTO seedboxes (hostname, provider_id, pm_service_id) VALUES (?, ?, ?)",
		hostname, providerID, serviceID)
	if err != nil {
		t.Fatalf("seed seedbox: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// putCache writes the "previous feed" rows a sync compares against.
func putCache(t *testing.T, database *sql.DB, providerID int64, services ...model.PMService) {
	t.Helper()
	st := &model.PMStore{DB: database}
	if err := st.Replace(context.Background(), providerID, services); err != nil {
		t.Fatalf("put cache: %v", err)
	}
}

func prevService(providerID, serviceID int64, cycle, nextDue string, amount float64, quotaBytes int64) model.PMService {
	return model.PMService{
		ProviderID: providerID, ServiceID: serviceID, Status: "Active",
		BillingCycle: cycle, NextDue: nextDue, RecurringAmount: amount,
		Currency: "EUR", DiskQuotaBytes: quotaBytes, SyncedAt: "2026-10-01T00:00:00Z",
	}
}

func readPricing(t *testing.T, database *sql.DB, seedboxID int64) (price float64, term int, currency, due string, ok bool) {
	t.Helper()
	var d sql.NullString
	err := database.QueryRow(`SELECT price, term, currency, next_due_date FROM pricings
		WHERE service_type = ? AND service_id = ? AND active = 1`,
		model.ServiceSeedbox, seedboxID).Scan(&price, &term, &currency, &d)
	if err == sql.ErrNoRows {
		return 0, 0, "", "", false
	}
	if err != nil {
		t.Fatalf("read pricing: %v", err)
	}
	return price, term, currency, d.String, true
}

func readDisk(t *testing.T, database *sql.DB, seedboxID int64) (int64, bool) {
	t.Helper()
	var d sql.NullInt64
	if err := database.QueryRow("SELECT disk_as_mb FROM seedboxes WHERE id = ?", seedboxID).Scan(&d); err != nil {
		t.Fatalf("read disk: %v", err)
	}
	return d.Int64, d.Valid
}

// The merge rule, in one table.
func TestEffective(t *testing.T) {
	cases := []struct {
		current, previous, next, want string
	}{
		// Unset inventory takes the feed value.
		{"", "anything", "2026-02-01", "2026-02-01"},
		{"", "", "", ""},
		// We wrote it last time, so the feed advances it.
		{"2026-01-01", "2026-01-01", "2026-02-01", "2026-02-01"},
		// A human changed it: keep theirs.
		{"2026-03-03", "2026-01-01", "2026-02-01", "2026-03-03"},
		// A human cleared it: the feed refills it.
		{"", "2026-01-01", "2026-02-01", "2026-02-01"},
	}
	for _, c := range cases {
		if got := Effective(c.current, c.previous, c.next); got != c.want {
			t.Errorf("Effective(%q, %q, %q) = %q, want %q",
				c.current, c.previous, c.next, got, c.want)
		}
	}
}

func TestSyncMatchesAndReconcilesNewBox(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "box.example.net")
	body := feedJSON(`{"id":101,"domain":"box.example.net","status":"Active","next_due":"2027-05-03",` +
		`"billing_cycle":"Quarterly","recurring_amount":44.97,` +
		`"seedbox":{"usage":{"disk_quota_bytes":4294967296}}}`)
	srv := mockFeed(t, body)
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := readLink(t, database, box); !got.Valid || got.Int64 != 101 {
		t.Fatalf("new box not linked in sync: %+v", got)
	}
	price, term, _, due, ok := readPricing(t, database, box)
	if !ok || price != 44.97 || term != model.TermQuarterly || due != "2027-05-03" {
		t.Fatalf("new box not reconciled: price=%v term=%d due=%q ok=%v", price, term, due, ok)
	}
	if disk, valid := readDisk(t, database, box); !valid || disk != 4096 {
		t.Fatalf("new box quota not reconciled: %d valid=%v", disk, valid)
	}
}

// The whole point of comparing against the previous feed value: a renewal
// that comes round again must move forward, not freeze at the first value.
func TestSyncAdvancesRenewal(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	// Previous sync wrote 2026-01-01; the renewal has since rolled over.
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2026-01-01", 10, 0))

	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "2026-02-01", 0)))
	res, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Synced != 1 || res.Matched != 1 {
		t.Fatalf("counts: synced=%d matched=%d", res.Synced, res.Matched)
	}

	_, _, _, due, ok := readPricing(t, database, box)
	if !ok {
		t.Fatal("expected a pricing row")
	}
	if due != "2026-02-01" {
		t.Fatalf("renewal must advance to 2026-02-01, got %q", due)
	}

	// Second sync with a later date must advance it again.
	srv2 := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "2026-03-01", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv2.URL, time.Now()); err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	_, _, _, due, _ = readPricing(t, database, box)
	if due != "2026-03-01" {
		t.Fatalf("second renewal must advance, got %q", due)
	}
}

func TestSyncKeepsHumanEdit(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2026-01-01", 10, 0))

	// A human pinned the due date to something the feed disagrees with.
	if _, err := database.Exec(`INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
		VALUES (?, ?, 'EUR', 12, 1, '2026-03-03')`, box, model.ServiceSeedbox); err != nil {
		t.Fatal(err)
	}

	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "2026-02-01", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	price, _, _, due, _ := readPricing(t, database, box)
	if due != "2026-03-03" {
		t.Fatalf("a human-edited due date must survive, got %q", due)
	}
	if price != 12 {
		t.Fatalf("a human-edited price must survive, got %v", price)
	}
}

// The dashboard's lazy due-date rollover must not read as a human edit on
// the next sync: AdvanceDueDates writes the advanced date back into the feed
// cache baseline, so provider corrections keep landing.
func TestSyncAdoptsFeedDueAfterDashboardAdvance(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2026-01-01", 10, 0))
	otherProvider := seedProviderKey(t, database, "other")
	putCache(t, database, otherProvider, prevService(otherProvider, 101, "Monthly", "2026-01-01", 10, 0))
	if _, err := database.Exec(`INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
		VALUES (?, ?, 'EUR', 10, 1, '2026-01-01')`, box, model.ServiceSeedbox); err != nil {
		t.Fatal(err)
	}
	if n, err := pricing.AdvanceDueDates(context.Background(), database); err != nil || n != 1 {
		t.Fatalf("advance: n=%d err=%v", n, err)
	}
	var advanced string
	if err := database.QueryRow("SELECT next_due_date FROM pricings WHERE service_id = ? AND service_type = ?",
		box, model.ServiceSeedbox).Scan(&advanced); err != nil {
		t.Fatal(err)
	}
	var baseline string
	if err := database.QueryRow("SELECT next_due FROM pm_services WHERE provider_id = ? AND service_id = 101",
		provider).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != advanced {
		t.Fatalf("rollover must move the merge baseline: cache=%q pricing=%q", baseline, advanced)
	}
	if err := database.QueryRow("SELECT next_due FROM pm_services WHERE provider_id = ? AND service_id = 101",
		otherProvider).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != "2026-01-01" {
		t.Fatalf("rollover changed another provider's baseline: %q", baseline)
	}

	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "2099-01-01", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, due, _ := readPricing(t, database, box); due != "2099-01-01" {
		t.Fatalf("feed due date frozen by the dashboard rollover: got %q, want 2099-01-01", due)
	}
}

func TestSyncKeepsHumanDueAnchorAfterDashboardAdvance(t *testing.T) {
	database := syncDB(t)
	ctx := context.Background()
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2000-01-01", 10, 0))
	// The user's 31st anchor has already been clamped by a February rollover.
	if _, err := database.Exec(`INSERT INTO pricings
		(service_id, service_type, currency, price, term, next_due_date, due_day)
		VALUES (?, ?, 'EUR', 10, 1, '2000-02-29', 31)`, box, model.ServiceSeedbox); err != nil {
		t.Fatal(err)
	}
	if n, err := pricing.AdvanceDueDates(ctx, database); err != nil || n != 1 {
		t.Fatalf("advance: n=%d err=%v", n, err)
	}
	_, _, _, advanced, _ := readPricing(t, database, box)
	var baseline string
	if err := database.QueryRow("SELECT next_due FROM pm_services WHERE provider_id = ? AND service_id = 101",
		provider).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != "2000-01-01" {
		t.Fatalf("user-edited pricing must leave the feed baseline alone: %q", baseline)
	}
	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "2099-01-01", 0)))
	if _, err := Sync(ctx, database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, due, _ := readPricing(t, database, box); due != advanced {
		t.Fatalf("sync replaced the user's advanced due date: got %q, want %q", due, advanced)
	}
	var day int
	if err := database.QueryRow("SELECT due_day FROM pricings WHERE service_id = ? AND service_type = ?",
		box, model.ServiceSeedbox).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if day != 31 {
		t.Fatalf("sync lost the user's month-end anchor: got %d, want 31", day)
	}
}

// A feed that briefly stops reporting the usage block must not erase the
// quota baseline — the next real quota change still has to land.
func TestSyncQuotaDropThenChangeStillApplies(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")

	const q1 = int64(1) << 40
	const q3 = int64(2) << 40
	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "", q1)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if disk, ok := readDisk(t, database, box); !ok || disk != q1/(1024*1024) {
		t.Fatalf("first quota: disk=%v ok=%v", disk, ok)
	}

	noUsage := feedJSON(`{"id":101,"product":"P101","status":"Active","next_due":"",
		"billing_cycle":"Monthly","recurring_amount":10}`)
	srv2 := mockFeed(t, noUsage)
	if _, err := Sync(context.Background(), database, provider, "k", srv2.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if disk, ok := readDisk(t, database, box); !ok || disk != q1/(1024*1024) {
		t.Fatalf("dropped usage must leave the inventory alone: disk=%v", disk)
	}
	var cachedQuota int64
	if err := database.QueryRow("SELECT disk_quota_bytes FROM pm_services WHERE provider_id = ? AND service_id = 101",
		provider).Scan(&cachedQuota); err != nil {
		t.Fatal(err)
	}
	if cachedQuota != q1 {
		t.Fatalf("dropped usage must keep the cache baseline: got %d, want %d", cachedQuota, q1)
	}

	srv3 := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "", q3)))
	if _, err := Sync(context.Background(), database, provider, "k", srv3.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	if disk, ok := readDisk(t, database, box); !ok || disk != q3/(1024*1024) {
		t.Fatalf("restored quota change ignored: disk=%d, want %d", disk, q3/(1024*1024))
	}
}

func TestSyncPreservesEditedBillingTerm(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2026-01-01", 10, 0))
	if _, err := database.Exec(`INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
		VALUES (?, ?, 'EUR', 10, ?, '2026-01-01')`, box, model.ServiceSeedbox, model.TermQuarterly); err != nil {
		t.Fatal(err)
	}
	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"11"`, "2026-02-01", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, term, _, _, _ := readPricing(t, database, box)
	if term != model.TermQuarterly {
		t.Fatalf("edited billing term overwritten: %d", term)
	}
}

func TestSyncFillsEmptyPriceAndTerm(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Quarterly", "", 0, 0))

	srv := mockFeed(t, feedJSON(svcJSON(101, "Quarterly", `"44.97"`, "2027-05-03", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	price, term, currency, due, ok := readPricing(t, database, box)
	if !ok {
		t.Fatal("expected a pricing row to be created")
	}
	if price != 44.97 || term != model.TermQuarterly || currency != "EUR" {
		t.Fatalf("price/term/currency = %v/%d/%q", price, term, currency)
	}
	// A price without its term reports a wrong monthly cost, so both land.
	if due != "2027-05-03" {
		t.Fatalf("due = %q", due)
	}
}

// An unmappable cycle must leave price and term alone: guessing would invent
// a monthly cost.
func TestSyncUnknownCycleWritesNoPrice(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "weird", "", 5, 0))
	if _, err := database.Exec(`INSERT INTO pricings (service_id, service_type, currency, price, term)
		VALUES (?, ?, 'EUR', 5, 1)`, box, model.ServiceSeedbox); err != nil {
		t.Fatal(err)
	}

	srv := mockFeed(t, feedJSON(svcJSON(101, "weird", `"99"`, "", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	price, term, _, _, ok := readPricing(t, database, box)
	if !ok {
		t.Fatal("the existing pricing row must be kept")
	}
	if price != 5 || term != model.TermMonthly {
		t.Fatalf("an unmappable cycle must not touch price/term, got %v/%d", price, term)
	}
}

func TestSyncWritesQuotaAsDisk(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "", 0, 0))

	const quota = 9910637035520 // bytes
	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "", quota)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	disk, ok := readDisk(t, database, box)
	if !ok {
		t.Fatal("disk should be set")
	}
	const wantMB = quota / (1024 * 1024) // inventory is whole MB, 1024-based
	if disk != wantMB {
		t.Fatalf("disk = %d MB, want %d", disk, wantMB)
	}
}

// A cycle idlerthing cannot map must skip the price, not the quota: the feed
// still reports disk usage for a service billed on an odd cycle, and the fleet
// view would otherwise show "no quota reported" forever.
func TestSyncUnknownCycleStillWritesQuota(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	putCache(t, database, provider, prevService(provider, 101, "Free Account", "", 0, 0))

	const quota = 9910637035520
	srv := mockFeed(t, feedJSON(svcJSON(101, "Free Account", `"0"`, "", quota)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	disk, ok := readDisk(t, database, box)
	if !ok {
		t.Fatal("an unmappable cycle must still record the quota")
	}
	if want := int64(quota / (1024 * 1024)); disk != want {
		t.Fatalf("disk = %d MB, want %d", disk, want)
	}
}

// A quota the feed did not report must not wipe a disk size the human set.
func TestSyncQuotaZeroWritesNoDisk(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedLinkedSeedbox(t, database, provider, 101, "box1.example.net")
	if _, err := database.Exec("UPDATE seedboxes SET disk_as_mb = 500 WHERE id = ?", box); err != nil {
		t.Fatal(err)
	}
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "", 0, 0))

	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	disk, _ := readDisk(t, database, box)
	if disk != 500 {
		t.Fatalf("a zero quota must write nothing, got %d", disk)
	}
}

func TestSyncQuotaPresence(t *testing.T) {
	const quota = int64(1) << 40
	for _, tc := range []struct {
		name, seedbox string
		want          int64
	}{
		{"missing seedbox", "", quota},
		{"missing usage", `,"seedbox":{}`, quota},
		{"explicit zero", `,"seedbox":{"usage":{"disk_quota_bytes":0}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := syncDB(t)
			provider := seedProviderKey(t, database, "k")
			putCache(t, database, provider, prevService(provider, 101, "Monthly", "", 10, quota))
			srv := mockFeed(t, feedJSON(`{"id":101,"status":"Active","billing_cycle":"Monthly","recurring_amount":10`+tc.seedbox+`}`))
			if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
				t.Fatal(err)
			}
			var got int64
			if err := database.QueryRow("SELECT disk_quota_bytes FROM pm_services WHERE provider_id = ? AND service_id = 101",
				provider).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("cached quota = %d, want %d", got, tc.want)
			}
		})
	}
}

// A rejected key must leave the cache intact: the last good data is better
// than none, and the next sync can recover.
func TestSyncAuthErrorKeepsCache(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2026-01-01", 10, 0))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Sync(context.Background(), database, provider, "bad", srv.URL, time.Now())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	got, err := (&model.PMStore{DB: database}).List(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NextDue != "2026-01-01" {
		t.Fatalf("cache must survive a rejected key, got %+v", got)
	}
}

// A transport failure is not a rejection, and must also keep the cache.
func TestSyncTransportErrorKeepsCache(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	putCache(t, database, provider, prevService(provider, 101, "Monthly", "2026-01-01", 10, 0))

	_, err := Sync(context.Background(), database, provider, "k", "http://127.0.0.1:1/nope", time.Now())
	if err == nil {
		t.Fatal("a transport failure must surface")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatal("a transport failure must not be reported as a rejected key")
	}
	got, _ := (&model.PMStore{DB: database}).List(context.Background(), provider)
	if len(got) != 1 {
		t.Fatalf("cache must survive a transport failure, got %+v", got)
	}
}

func TestSyncReplacesCacheAndStampsTime(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	putCache(t, database, provider, prevService(provider, 999, "Monthly", "", 1, 0))

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "", 0)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got, err := (&model.PMStore{DB: database}).List(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ServiceID != 101 {
		t.Fatalf("cache must be replaced by the feed, got %+v", got)
	}
	if got[0].SyncedAt != now.Format(time.RFC3339) {
		t.Fatalf("synced_at = %q", got[0].SyncedAt)
	}
}

// An unlinked seedbox is not the sync's business: it must not be touched.
func TestSyncIgnoresUnlinkedSeedbox(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	// A seedbox of the same provider with no feed link.
	res, err := database.Exec("INSERT INTO seedboxes (hostname, provider_id) VALUES (?, ?)",
		"unlinked.example.net", provider)
	if err != nil {
		t.Fatal(err)
	}
	unlinked, _ := res.LastInsertId()

	srv := mockFeed(t, feedJSON(svcJSON(101, "Monthly", `"10"`, "", 12345)))
	if _, err := Sync(context.Background(), database, provider, "k", srv.URL, time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, _, _, _, ok := readPricing(t, database, unlinked); ok {
		t.Fatal("an unlinked seedbox must not gain pricing")
	}
	if _, ok := readDisk(t, database, unlinked); ok {
		t.Fatal("an unlinked seedbox must not gain a disk size")
	}
}
