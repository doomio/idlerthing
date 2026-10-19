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

package model

import (
	"context"
	"database/sql"
	"testing"
)

func samplePMServices() []PMService {
	return []PMService{
		{
			ServiceID:        101,
			Product:          "Dragon-R",
			Group:            "Seedbox",
			Domain:           "doom.example.net",
			Status:           "Active",
			Registered:       "2025-03-01",
			NextDue:          "2026-11-01",
			BillingCycle:     "Monthly",
			RecurringAmount:  11.94,
			Currency:         "EUR",
			Username:         "doom",
			LoginURL:         "https://example.net/doom/",
			PermanentAddress: "doom.example.net",
			TrackedSince:     "2026-10-01",
			SentBytes:        1 << 40,
			ReceivedBytes:    1 << 39,
			CPUHours:         12,
			CPUSince:         "2026-10-01",
			DiskUsedBytes:    2 << 40,
			DiskQuotaBytes:   4 << 40,
			Torrents:         310,
			GeneratedAt:      "2026-10-18T00:00:00Z",
			SyncedAt:         "2026-10-18T00:05:00Z",
		},
		{
			ServiceID:       102,
			Product:         "M1000",
			Status:          "Cancelled",
			BillingCycle:    "Annually",
			RecurringAmount: 99,
			Currency:        "USD",
			SyncedAt:        "2026-10-18T00:05:00Z",
		},
	}
}

func TestPMStoreReplaceIsWholesale(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &PMStore{DB: database}

	if err := st.Replace(ctx, 7, samplePMServices()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := st.List(ctx, 7)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("first replace: want 2 services, got %d", len(got))
	}
	if got[0].ServiceID != 101 || got[1].ServiceID != 102 {
		t.Fatalf("want ascending service ids, got %d then %d", got[0].ServiceID, got[1].ServiceID)
	}

	// A second sync returning only one service must drop the other.
	if err := st.Replace(ctx, 7, samplePMServices()[:1]); err != nil {
		t.Fatalf("Replace 2: %v", err)
	}
	got, err = st.List(ctx, 7)
	if err != nil {
		t.Fatalf("List 2: %v", err)
	}
	if len(got) != 1 || got[0].ServiceID != 101 {
		t.Fatalf("second replace must be wholesale, got %+v", got)
	}
}

func TestPMStoreRoundTripsEveryField(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &PMStore{DB: database}

	want := samplePMServices()[0]
	want.ProviderID = 7 // set by Replace, not by the caller's slice
	if err := st.Replace(ctx, 7, []PMService{want}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := st.List(ctx, 7)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 service, got %d", len(got))
	}
	if got[0] != want {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got[0], want)
	}
}

func TestPMStoreReplaceIsPerProvider(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &PMStore{DB: database}

	if err := st.Replace(ctx, 1, samplePMServices()[:1]); err != nil {
		t.Fatalf("Replace provider 1: %v", err)
	}
	if err := st.Replace(ctx, 2, samplePMServices()[1:]); err != nil {
		t.Fatalf("Replace provider 2: %v", err)
	}

	one, err := st.List(ctx, 1)
	if err != nil {
		t.Fatalf("List 1: %v", err)
	}
	if len(one) != 1 || one[0].ServiceID != 101 {
		t.Fatalf("provider 1 rows clobbered by provider 2: %+v", one)
	}
	two, err := st.List(ctx, 2)
	if err != nil {
		t.Fatalf("List 2: %v", err)
	}
	if len(two) != 1 || two[0].ServiceID != 102 {
		t.Fatalf("provider 2 rows wrong: %+v", two)
	}
}

func TestProviderKeyRoundTrip(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &CatalogStore{DB: database}

	id, err := st.Create(ctx, Catalogs["providers"], "Example Host")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if has, err := st.HasProviderKey(ctx, id); err != nil || has {
		t.Fatalf("a new provider must have no key (has=%v err=%v)", has, err)
	}
	if k, err := st.ProviderKey(ctx, id); err != nil || k != "" {
		t.Fatalf("empty key expected (k=%q err=%v)", k, err)
	}

	if err := st.SetProviderKey(ctx, id, "pmapi-secret"); err != nil {
		t.Fatalf("SetProviderKey: %v", err)
	}
	if k, err := st.ProviderKey(ctx, id); err != nil || k != "pmapi-secret" {
		t.Fatalf("key round trip (k=%q err=%v)", k, err)
	}
	if has, err := st.HasProviderKey(ctx, id); err != nil || !has {
		t.Fatalf("HasProviderKey (has=%v err=%v)", has, err)
	}

	// The generic catalog listing must not have grown a secret.
	items, err := st.List(ctx, Catalogs["providers"])
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Name != "Example Host" {
		t.Fatalf("List must return name-only items, got %+v", items)
	}

	if err := st.SetProviderKey(ctx, id, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if has, err := st.HasProviderKey(ctx, id); err != nil || has {
		t.Fatalf("cleared key must report absent (has=%v err=%v)", has, err)
	}
}

func TestProvidersWithKey(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &CatalogStore{DB: database}

	withKey, err := st.Create(ctx, Catalogs["providers"], "Has Key")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Create(ctx, Catalogs["providers"], "No Key"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.SetProviderKey(ctx, withKey, "k"); err != nil {
		t.Fatalf("SetProviderKey: %v", err)
	}

	providers, err := st.ProvidersWithKey(ctx)
	if err != nil {
		t.Fatalf("ProvidersWithKey: %v", err)
	}
	if len(providers) != 1 || providers[0].ID != withKey {
		t.Fatalf("want exactly the keyed provider, got %v", providers)
	}
	// The name is what makes a sync failure attributable.
	if providers[0].Name != "Has Key" {
		t.Errorf("provider name = %q, want %q", providers[0].Name, "Has Key")
	}
}

func TestPMStoreReplaceEmptyClears(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &PMStore{DB: database}

	if err := st.Replace(ctx, 7, samplePMServices()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := st.Replace(ctx, 7, nil); err != nil {
		t.Fatalf("Replace nil: %v", err)
	}
	got, err := st.List(ctx, 7)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty replace must clear, got %+v", got)
	}
}

func TestBumpFeedDueBaselineWithoutCompleteLink(t *testing.T) {
	for _, tc := range []struct {
		name              string
		provider, service sql.NullInt64
	}{
		{"no provider", sql.NullInt64{}, sql.NullInt64{Int64: 101, Valid: true}},
		{"no service", sql.NullInt64{Int64: 7, Valid: true}, sql.NullInt64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := testDB(t)
			ctx := context.Background()
			if _, err := database.Exec("INSERT INTO providers (id, name) VALUES (7, 'Example Host')"); err != nil {
				t.Fatal(err)
			}
			service := samplePMServices()[0]
			if err := (&PMStore{DB: database}).Replace(ctx, 7, []PMService{service}); err != nil {
				t.Fatal(err)
			}
			res, err := database.Exec("INSERT INTO seedboxes (hostname, provider_id, pm_service_id) VALUES ('box.example.net', ?, ?)",
				tc.provider, tc.service)
			if err != nil {
				t.Fatal(err)
			}
			box, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			tx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := BumpFeedDueBaselineTx(ctx, tx, box, service.NextDue, "2026-12-01"); err != nil {
				t.Fatal(err)
			}
			var due string
			if err := tx.QueryRowContext(ctx, "SELECT next_due FROM pm_services WHERE provider_id = 7 AND service_id = 101").Scan(&due); err != nil {
				t.Fatal(err)
			}
			if due != service.NextDue {
				t.Fatalf("incomplete link changed the feed baseline: %q", due)
			}
		})
	}
}
