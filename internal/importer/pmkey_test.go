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

package importer

import (
	"context"
	"strings"
	"testing"
)

// A provider API key must never be imported. An export carries the key only
// if it leaked it in the first place, but a hand-edited or third-party
// document can contain one too, and silently adopting someone else's key
// would be worse than ignoring it.
func TestImportIgnoresProviderAPIKey(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"providers":[
	  {"id":1,"name":"Example Host","pm_api_key":"imported-secret"}]}`

	if _, err := Import(context.Background(), database, strings.NewReader(doc), false); err != nil {
		t.Fatalf("import: %v", err)
	}

	var name, key string
	if err := database.QueryRow("SELECT name, pm_api_key FROM providers").Scan(&name, &key); err != nil {
		t.Fatalf("read provider: %v", err)
	}
	if name != "Example Host" {
		t.Fatalf("provider name not imported: %q", name)
	}
	if key != "" {
		t.Fatalf("imported provider API key must be discarded, got %q", key)
	}
}

// An existing key survives a re-import of the same provider: the importer
// merges catalogs by name and must not clear the column on the way through.
func TestImportKeepsExistingProviderKey(t *testing.T) {
	database := testDB(t)
	if _, err := database.Exec(
		"INSERT INTO providers (name, pm_api_key) VALUES (?, ?)", "Example Host", "existing-secret"); err != nil {
		t.Fatal(err)
	}

	doc := `{"format":1,"providers":[{"id":1,"name":"Example Host"}]}`
	if _, err := Import(context.Background(), database, strings.NewReader(doc), false); err != nil {
		t.Fatalf("import: %v", err)
	}

	var n int
	var key string
	if err := database.QueryRow(
		"SELECT COUNT(*), COALESCE(MAX(pm_api_key), '') FROM providers").Scan(&n, &key); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the catalog row to merge, got %d rows", n)
	}
	if key != "existing-secret" {
		t.Fatalf("re-import must not clear the stored key, got %q", key)
	}
}
