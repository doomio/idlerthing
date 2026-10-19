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
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func TestSettingsImport(t *testing.T) {
	ts, db := newTestServer(t)
	client := newClient(t)
	upload := func(token, filename, content string) *http.Response {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("csrf_token", token)
		f, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, content)
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/settings/import", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	check := func(want int) {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM servers").Scan(&n); err != nil || n != want {
			t.Fatalf("servers=%d error=%v, want %d", n, err, want)
		}
	}
	resp := upload("", "servers.csv", "hostname,server_type_name\nalpha,Dedicated\n")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	check(0)
	resp = login(t, client, ts, testPassword)
	resp.Body.Close()
	resp, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(html, []byte(`enctype="multipart/form-data"`)) {
		t.Fatal("missing upload form")
	}
	token := csrfRe.FindSubmatch(html)[1]
	resp = upload("wrong", "servers.csv", "hostname,server_type_name\nalpha,Dedicated\n")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	check(0)
	csv := "hostname,server_type_name\nalpha,Dedicated\n"
	resp = upload(string(token), "servers.csv", csv)
	if resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("CSV status=%d body=%s", resp.StatusCode, b)
	}
	resp.Body.Close()
	check(1)
	resp = upload(string(token), "servers.csv", csv)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("duplicate status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	check(1)
	var hostname string
	if err := db.QueryRow("SELECT hostname FROM servers LIMIT 1").Scan(&hostname); err != nil || hostname != "alpha" {
		t.Fatalf("existing server changed: %q, %v", hostname, err)
	}
	resp = upload(string(token), "my.json", `[{"hostname":"beta","server_type_name":"Dedicated"}]`)
	if resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("my-idlers JSON status=%d body=%s", resp.StatusCode, b)
	}
	resp.Body.Close()
	check(2)
	resp = upload(string(token), "native.json", `{"format":1,"servers":[{"server":{"id":1,"hostname":"gamma","server_type":1}}]}`)
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("native import duplicated existing inventory")
	}
	resp.Body.Close()
	check(2)
	resp = upload(string(token), "invalid.json", `{"format":2}`)
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("invalid import accepted")
	}
	resp.Body.Close()
	check(2)
	resp = upload(string(token), "huge.csv", "hostname,server_type_name\n"+strings.Repeat("x", 11<<20)+",Dedicated\n")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	check(2)
}
