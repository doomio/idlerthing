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
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

// The provider API key is a write-only secret. It is stored on a row of a
// name-only catalog whose every read path is generic, so each of those paths
// has to be proven not to carry it.

const testProviderKey = "pmapi-secret-4c1f9a"

// seedProviderWithKey inserts a provider holding key and returns its id.
func seedProviderWithKey(t *testing.T, database *sql.DB, key string) int64 {
	t.Helper()
	st := &model.CatalogStore{DB: database}
	ctx := context.Background()
	id, err := st.Create(ctx, model.Catalogs["providers"], "Example Host")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.SetProviderKey(ctx, id, key); err != nil {
		t.Fatalf("set provider key: %v", err)
	}
	return id
}

func assertNoKey(t *testing.T, what, body, key string) {
	t.Helper()
	if strings.Contains(body, key) {
		t.Fatalf("%s leaked the provider API key", what)
	}
}

func TestCatalogPageOmitsProviderKey(t *testing.T) {
	ts, database := newTestServer(t)
	seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/catalogs/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /catalogs/providers: %d", resp.StatusCode)
	}
	assertNoKey(t, "the provider catalog page", readBody(t, resp), testProviderKey)
}

func TestExportJSONOmitsProviderKey(t *testing.T) {
	ts, database := newTestServer(t)
	seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/export/json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /export/json: %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	assertNoKey(t, "the JSON export", body, testProviderKey)
	// The provider itself must still be exported — the key is redacted, not
	// the whole feature.
	if !strings.Contains(body, "Example Host") {
		t.Fatal("the JSON export must still contain the provider name")
	}
}

func TestExportCSVOmitsProviderKey(t *testing.T) {
	ts, database := newTestServer(t)
	seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/export/csv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /export/csv: %d", resp.StatusCode)
	}
	// The zip is binary; a raw byte search still catches a leaked key.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	assertNoKey(t, "the CSV export", string(raw), testProviderKey)
}

func TestAPICatalogOmitsProviderKey(t *testing.T) {
	ts, database := newTestServer(t)
	seedProviderWithKey(t, database, testProviderKey)
	token := setAPIToken(t, database)

	req, _ := http.NewRequest("GET", ts.URL+"/api/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/providers: %d", resp.StatusCode)
	}
	assertNoKey(t, "GET /api/providers", readBody(t, resp), testProviderKey)
}

// keyURL builds the key-route path for a catalog entry.
func keyURL(kind string, id int64) string {
	return "/catalogs/" + kind + "/" + strconv.FormatInt(id, 10) + "/key"
}

// The key form follows the write-only secret pattern: a blank field means
// "keep the stored key", and only an explicit clear removes it.
func TestProviderKeyFormBlankKeepsExisting(t *testing.T) {
	ts, database := newTestServer(t)
	id := seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, keyURL("providers", id), url.Values{
		"pm_api_key": {""},
	})
	resp.Body.Close()

	st := &model.CatalogStore{DB: database}
	if k, err := st.ProviderKey(context.Background(), id); err != nil || k != testProviderKey {
		t.Fatalf("a blank field must keep the stored key (k=%q err=%v)", k, err)
	}
}

func TestProviderKeyFormReplaces(t *testing.T) {
	ts, database := newTestServer(t)
	id := seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, keyURL("providers", id), url.Values{
		"pm_api_key": {"replacement-key"},
	})
	resp.Body.Close()

	st := &model.CatalogStore{DB: database}
	if k, err := st.ProviderKey(context.Background(), id); err != nil || k != "replacement-key" {
		t.Fatalf("a filled field must replace the key (k=%q err=%v)", k, err)
	}
}

func TestProviderKeyFormClear(t *testing.T) {
	ts, database := newTestServer(t)
	id := seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, keyURL("providers", id), url.Values{
		"pm_api_key": {""},
		"clear_key":  {"on"},
	})
	resp.Body.Close()

	st := &model.CatalogStore{DB: database}
	if k, err := st.ProviderKey(context.Background(), id); err != nil || k != "" {
		t.Fatalf("the clear action must remove the key (k=%q err=%v)", k, err)
	}
}

// The key route only exists for providers: another catalog must 404 rather
// than silently accept a secret it has no column for.
func TestProviderKeyRouteRefusedForOtherCatalogs(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	st := &model.CatalogStore{DB: database}
	id, err := st.Create(context.Background(), model.Catalogs["locations"], "Somewhere")
	if err != nil {
		t.Fatal(err)
	}
	resp := postForm(t, client, ts, keyURL("locations", id), url.Values{
		"pm_api_key": {"x"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a non-provider catalog, got %d", resp.StatusCode)
	}
}

// The key must never be echoed back into the form, not even as a value or in
// an error message.
func TestProviderKeyFormNeverEchoesKey(t *testing.T) {
	ts, database := newTestServer(t)
	seedProviderWithKey(t, database, testProviderKey)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/catalogs/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	assertNoKey(t, "the provider form", body, testProviderKey)
	if !strings.Contains(body, "pm_api_key") {
		t.Fatal("the provider form must offer a key field")
	}
}
