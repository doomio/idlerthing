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
	"testing"

	"idlerthing/internal/model"
)

// seedService writes one cached feed service directly.
func seedService(t *testing.T, database *sql.DB, providerID int64, s model.PMService) {
	t.Helper()
	s.ProviderID = providerID
	if s.SyncedAt == "" {
		s.SyncedAt = "2026-10-18T00:00:00Z"
	}
	if err := (&model.PMStore{DB: database}).Replace(context.Background(), providerID, []model.PMService{s}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

func seedSeedbox(t *testing.T, database *sql.DB, providerID int64, hostname string) int64 {
	t.Helper()
	res, err := database.Exec("INSERT INTO seedboxes (hostname, provider_id) VALUES (?, ?)",
		hostname, providerID)
	if err != nil {
		t.Fatalf("seed seedbox: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func readLink(t *testing.T, database *sql.DB, seedboxID int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := database.QueryRow("SELECT pm_service_id FROM seedboxes WHERE id = ?", seedboxID).Scan(&v); err != nil {
		t.Fatalf("read link: %v", err)
	}
	return v
}

func TestMatchByDomain(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "box1.example.net")
	seedService(t, database, provider, model.PMService{
		ServiceID: 101, Domain: "box1.example.net", PermanentAddress: "http://abcd.example.net/",
	})

	n, err := Match(context.Background(), database, provider)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if n != 1 {
		t.Fatalf("matched %d, want 1", n)
	}
	if got := readLink(t, database, box); !got.Valid || got.Int64 != 101 {
		t.Fatalf("link = %+v, want service 101", got)
	}
}

func TestMatchByPermanentAddressHost(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "box2.example.net")
	// The permanent address is a URL; only its host is compared.
	seedService(t, database, provider, model.PMService{
		ServiceID: 202, Domain: "other.example.net", PermanentAddress: "http://box2.example.net/",
	})

	if _, err := Match(context.Background(), database, provider); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := readLink(t, database, box); !got.Valid || got.Int64 != 202 {
		t.Fatalf("link = %+v, want service 202", got)
	}
}

func TestMatchDoesNotGuessFromUsername(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "exuser.box3.example.net")
	seedService(t, database, provider, model.PMService{
		ServiceID: 303, Domain: "unrelated.example.net", Username: "exuser",
	})

	if _, err := Match(context.Background(), database, provider); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := readLink(t, database, box); got.Valid {
		t.Fatalf("username substring is not proof of ownership: %+v", got)
	}
}

// A provider can carry other services: one feed service does not prove which
// inventory seedbox it belongs to.
func TestMatchSingleServiceAccount(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "nothing-in-common.example.net")
	seedService(t, database, provider, model.PMService{
		ServiceID: 404, Domain: "elsewhere.example.net", Username: "someoneelse",
	})

	if _, err := Match(context.Background(), database, provider); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := readLink(t, database, box); got.Valid {
		t.Fatalf("single service is not proof of ownership: %+v", got)
	}
}

// Two services claiming the same hostname are also ambiguous; a wrong link
// writes another service's renewal date onto this box.
func TestMatchRefusesConflictingExactHosts(t *testing.T) {
	services := []model.PMService{
		{ServiceID: 1, Domain: "box.example.net"},
		{ServiceID: 2, PermanentAddress: "https://box.example.net/"},
	}
	if id, ok := matchService("box.example.net", services); ok || id != 0 {
		t.Fatalf("ambiguous exact hosts linked to %d", id)
	}
}

func TestMatchNoGuessOnAmbiguity(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "ambiguous.example.net")
	// Both seeded in one replace so the provider really has two services.
	if err := (&model.PMStore{DB: database}).Replace(context.Background(), provider, []model.PMService{
		{ProviderID: provider, ServiceID: 501, Domain: "a.example.net", Username: "aaa", SyncedAt: "x"},
		{ProviderID: provider, ServiceID: 502, Domain: "b.example.net", Username: "bbb", SyncedAt: "x"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Match(context.Background(), database, provider); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := readLink(t, database, box); got.Valid {
		t.Fatalf("ambiguous match must stay unlinked, got service %d", got.Int64)
	}
}

// A link the user made by hand must never be silently rewritten.
func TestMatchDoesNotOverwrite(t *testing.T) {
	database := syncDB(t)
	provider := seedProviderKey(t, database, "k")
	box := seedSeedbox(t, database, provider, "box1.example.net")
	if _, err := database.Exec("UPDATE seedboxes SET pm_service_id = 999 WHERE id = ?", box); err != nil {
		t.Fatal(err)
	}
	seedService(t, database, provider, model.PMService{
		ServiceID: 101, Domain: "box1.example.net",
	})

	if _, err := Match(context.Background(), database, provider); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := readLink(t, database, box); !got.Valid || got.Int64 != 999 {
		t.Fatalf("an existing link must be left alone, got %+v", got)
	}
}

// Cost of a wrong match is a wrong renewal date, so a link to a service the
// feed no longer lists is not repaired by guessing.
func TestMatchIgnoresSeedboxOfAnotherProvider(t *testing.T) {
	database := syncDB(t)
	mine := seedProviderKey(t, database, "k")
	other := seedProviderKey(t, database, "k2")
	otherBox := seedSeedbox(t, database, other, "box1.example.net")
	seedService(t, database, mine, model.PMService{ServiceID: 101, Domain: "box1.example.net"})

	if _, err := Match(context.Background(), database, mine); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := readLink(t, database, otherBox); got.Valid {
		t.Fatalf("another provider's seedbox must not be linked, got %d", got.Int64)
	}
}
