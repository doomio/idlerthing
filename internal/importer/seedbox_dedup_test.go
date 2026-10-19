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

func TestNativeImportPreservesManualOverride(t *testing.T) {
	db := testDB(t)
	doc := `{"format":1,"seedboxes":[{"seedbox":{"id":1,"hostname":"trophy.example.net","pm_manual_override":true}}]}`
	if _, err := Import(context.Background(), db, strings.NewReader(doc), false); err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := db.QueryRow("SELECT pm_manual_override FROM seedboxes WHERE hostname='trophy.example.net'").Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("restored seedbox lost manual override")
	}
}

func TestNativeImportSkipsDuplicateSeedboxes(t *testing.T) {
	db := testDB(t)
	doc := `{"format":1,"seedboxes":[{"seedbox":{"id":1,"hostname":"Box.Example","title":"first"}},{"seedbox":{"id":2,"hostname":"box.example","title":"second"}}]}`
	sum, err := Import(context.Background(), db, strings.NewReader(doc), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Seedboxes != 1 {
		t.Fatalf("inserted %d seedboxes, want 1", sum.Seedboxes)
	}
	var n int
	var title string
	if err := db.QueryRow("SELECT COUNT(*), title FROM seedboxes").Scan(&n, &title); err != nil {
		t.Fatal(err)
	}
	if n != 1 || title != "first" {
		t.Fatalf("rows=%d title=%q", n, title)
	}
}
