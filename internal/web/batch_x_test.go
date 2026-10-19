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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Batch X1 — DNS records linked to shared or reseller hosting show on that
// hosting's detail page (the card used to exist for servers/domains only).
func TestDNSCardOnHostingDetail(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	postForm(t, client, ts, "/shared", url.Values{"main_domain": {"shared.example.com"}, "active": {"on"}}).Body.Close()
	postForm(t, client, ts, "/reseller", url.Values{"main_domain": {"reseller.example.com"}, "active": {"on"}}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"www.shared.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.1"}, "shared_id": {"1"},
	}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"mail.reseller.example.com"}, "dns_type": {"MX"}, "address": {"203.0.113.2"}, "reseller_id": {"1"},
	}).Body.Close()

	for path, want := range map[string]string{
		"/shared/1":   "www.shared.example.com",
		"/reseller/1": "mail.reseller.example.com",
	} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if !strings.Contains(body, want) {
			t.Errorf("%s should list its DNS record %q", path, want)
		}
	}
	// Misc services cannot carry DNS records: no card at all.
	postForm(t, client, ts, "/misc", url.Values{"name": {"thing"}, "active": {"on"}}).Body.Close()
	resp, _ := client.Get(ts.URL + "/misc/1")
	body := readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "No DNS records linked") {
		t.Fatal("misc detail must not render a DNS card")
	}
}

// Batch X2 — a zoned IPv6 address is refused on create: the zone is scope,
// not address, and the '%' would break every later whois request URL.
func TestIPCreateRejectsZone(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "zone-host")
	resp := postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "address": {"fe80::1%eth0"},
	})
	resp.Body.Close()
	var n int
	database.QueryRow("SELECT COUNT(*) FROM ips").Scan(&n)
	if n != 0 {
		t.Fatalf("zoned address stored: %d rows", n)
	}
	page, _ := client.Get(ts.URL + "/ips")
	body := readBody(t, page)
	page.Body.Close()
	if !strings.Contains(body, "Invalid IP address") {
		t.Fatal("expected the invalid-address flash")
	}
}

// Batch X3 — the Prometheus "Test" button tests the SAVED url even while
// the feature is switched off; that is when you want to validate it.
func TestPrometheusTestUsesSavedURLWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	promSrv := httptest.NewServer(http.HandlerFunc(fakePromHandler))
	defer promSrv.Close()

	postForm(t, client, ts, "/settings", url.Values{
		"default_currency": {"USD"}, "dashboard_currency": {"USD"},
		"due_soon_amount": {"14"}, "recently_added_amount": {"5"}, "theme": {"dark"},
		"prometheus_url": {promSrv.URL}, // enable box left unchecked
	}).Body.Close()
	postForm(t, client, ts, "/settings/prometheus/test", url.Values{}).Body.Close()
	page, _ := client.Get(ts.URL + "/settings")
	body := readBody(t, page)
	page.Body.Close()
	if strings.Contains(body, "No Prometheus URL configured") {
		t.Fatal("test button must use the saved URL even when prometheus is disabled")
	}
	if !strings.Contains(body, "Connected") {
		t.Fatalf("expected a connection result flash, got:\n%s", body)
	}
}

// Batch X4 — the ping exec has an overall deadline: a binary that hangs
// (stalled name resolution) returns promptly instead of holding the
// handler for the resolver's whole retry budget.
func TestPingExecTimeout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ping")
	// The stub keeps a CHILD holding stdout open after the shell is killed —
	// the shape that pins CombinedOutput without WaitDelay.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origBin, origTimeout := pingBinary, pingTimeout
	pingBinary, pingTimeout = stub, 200*time.Millisecond
	defer func() { pingBinary, pingTimeout = origBin, origTimeout }()

	start := time.Now()
	_, err := execPing("127.0.0.1")
	if err == nil {
		t.Fatal("hung ping should error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ping deadline not enforced: took %v", elapsed)
	}
}

// A bare test Server has no knownIPs set; the methods must tolerate nil.
func TestKnownIPsNilSafe(t *testing.T) {
	var k *knownIPs
	k.add("1.2.3.4")
	if k.has("1.2.3.4") {
		t.Fatal("nil set must report nothing known")
	}
}

// Creating a catalog entry whose name already exists is reported as a
// conflict (via model.ErrConflict), not a generic save failure.
func TestCatalogCreateDuplicateFlash(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"Hetzner"}}).Body.Close()
	postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"Hetzner"}}).Body.Close()
	resp, err := client.Get(ts.URL + "/catalogs/providers")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "That name already exists.") {
		t.Fatalf("expected the conflict flash, got:\n%s", body)
	}
	if strings.Contains(body, "Save failed.") {
		t.Fatal("a conflict must not be reported as a generic failure")
	}
}

// A ping binary path that does not exist reports "not available", the
// state the systemd hardening notes describe, rather than "unreachable".
func TestPingBinaryMissing(t *testing.T) {
	orig := pingBinary
	pingBinary = filepath.Join(t.TempDir(), "no-such-ping")
	defer func() { pingBinary = orig }()
	_, err := execPing("127.0.0.1")
	if !errors.Is(err, errPingUnavailable) {
		t.Fatalf("want errPingUnavailable, got %v", err)
	}
}

// Editing a DNS record that is linked to hosting must keep the link: the
// edit form has no shared/reseller picker, so it carries the existing link
// through a hidden input — without it, Update writes NULL and the record is
// silently unlinked on every unrelated edit.
func TestDNSEditPreservesHostingLink(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	postForm(t, client, ts, "/shared", url.Values{"main_domain": {"links.example.com"}, "active": {"on"}}).Body.Close()
	postForm(t, client, ts, "/reseller", url.Values{"main_domain": {"res-links.example.com"}, "active": {"on"}}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"a.links.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.9"}, "shared_id": {"1"},
	}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"mx.res-links.example.com"}, "dns_type": {"MX"}, "address": {"203.0.113.10"}, "reseller_id": {"1"},
	}).Body.Close()

	cases := []struct {
		id    int
		field string
	}{{1, "shared_id"}, {2, "reseller_id"}}
	for _, c := range cases {
		resp, err := client.Get(ts.URL + "/dns/" + strconv.Itoa(c.id) + "/edit")
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		want := `name="` + c.field + `" value="1"`
		if !strings.Contains(body, want) {
			t.Fatalf("edit form for dns/%d missing hidden input %s", c.id, want)
		}
		postForm(t, client, ts, "/dns/"+strconv.Itoa(c.id)+"/update", url.Values{
			"hostname": {"renamed.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.11"},
			c.field: {"1"},
		}).Body.Close()
		var shared, reseller int64
		if err := database.QueryRow("SELECT COALESCE(shared_id,-1), COALESCE(reseller_id,-1) FROM dns WHERE id = ?", c.id).Scan(&shared, &reseller); err != nil {
			t.Fatal(err)
		}
		linked := shared
		if c.field == "reseller_id" {
			linked = reseller
		}
		if linked != 1 {
			t.Errorf("dns/%d: %s = %d after edit, want link preserved (1)", c.id, c.field, linked)
		}
	}
}
