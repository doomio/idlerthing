package importer

import (
	"context"
	"strings"
	"testing"
)

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
