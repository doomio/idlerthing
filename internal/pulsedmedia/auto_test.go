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
		fmt.Fprint(w, feedJSON(svcJSON(101, "Monthly", "10", "2027-01-01", 1073741824), strings.Replace(svcJSON(102, "Monthly", "20", "2027-01-01", 1073741824), `"status":"Active"`, `"status":"Active","domain":"new.example.net"`, 1)))
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
