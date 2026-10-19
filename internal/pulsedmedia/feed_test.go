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
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

func TestParseRealFeed(t *testing.T) {
	body, err := os.ReadFile("testdata/feed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Currency != "EUR" || f.ClientID != 54311 {
		t.Fatalf("bad envelope: %+v", f)
	}
	if len(f.Services) != 2 {
		t.Fatalf("want 2 services, got %d", len(f.Services))
	}

	s := f.Services[0]
	if s.ID != 72645 || s.Product != "Dragon-R" || s.Status != "Active" {
		t.Fatalf("bad service: %+v", s)
	}
	// A string amount must decode to a number.
	if math.Abs(float64(s.RecurringAmount)-44.97) > 1e-9 {
		t.Fatalf("recurring amount = %v", s.RecurringAmount)
	}
	if m := s.Monthly(); math.Abs(m-14.99) > 0.001 {
		t.Fatalf("quarterly 44.97 should normalise to 14.99/month, got %v", m)
	}
	if s.Username() != "exuser" {
		t.Fatalf("username = %q", s.Username())
	}
	if s.Seedbox == nil || s.Seedbox.Usage == nil {
		t.Fatal("usage block missing")
	}
	if s.Seedbox.Usage.Torrents != 7 || s.Seedbox.Usage.DiskQuotaBytes != 9910637035520 {
		t.Fatalf("bad usage: %+v", s.Seedbox.Usage)
	}

	// A service with no seedbox block and a non-recurring cycle.
	last := f.Services[1]
	if last.Username() != "" || last.Monthly() != 0 {
		t.Fatalf("free service should normalise to 0: %+v", last)
	}
	if last.Seedbox != nil {
		t.Fatal("seedbox block should be nil when absent")
	}
}

func TestParseHTMLBody(t *testing.T) {
	// A wrong URL or captive portal returns a page, not JSON: must error,
	// not panic.
	if _, err := Parse([]byte("<!doctype html><html><body>nope</body></html>")); err == nil {
		t.Fatal("HTML body must fail to parse")
	}
	if _, err := Parse([]byte("")); err == nil {
		t.Fatal("empty body must fail to parse")
	}
}

func TestAmountParsing(t *testing.T) {
	// Quoted and bare values, plus separators in both conventions.
	cases := []struct {
		raw  string
		want float64
	}{
		{`"11.94"`, 11.94},
		{`"€11.94"`, 11.94},
		{`"1,234.56"`, 1234.56},
		{`"11,94"`, 11.94},
		{`"1,234"`, 1234},
		{`12.5`, 12.5},
		{`""`, 0},
		{`"abc"`, 0},
		{`"-5.5"`, -5.5},
		// Exponent notation must keep its exponent, bare or quoted (the
		// character strip used to eat the 'e': 1e2 read as 12).
		{`1e2`, 100},
		{`"1e2"`, 100},
		// \u escapes decode before parsing ("\u20ac" is €; raw text leaked
		// the escape's hex digits into the amount: 2011.94).
		{`"\u20ac11.94"`, 11.94},
		{`"\u00a51,200"`, 1200},
		// Absurd magnitudes clamp to 0 on every path.
		{`2e12`, 0},
		{`"2e12"`, 0},
		{`1e400`, 0},
	}
	for _, c := range cases {
		body := []byte(`{"services":[{"id":1,"recurring_amount":` + c.raw + `}]}`)
		f, err := Parse(body)
		if err != nil {
			t.Fatalf("%s: Parse: %v", c.raw, err)
		}
		if got := float64(f.Services[0].RecurringAmount); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v want %v", c.raw, got, c.want)
		}
	}

	// One odd amount must not reject the whole feed and every renewal date
	// with it.
	if _, err := Parse([]byte(`{"services":[{"id":1,"recurring_amount":true}]}`)); err != nil {
		t.Fatalf("odd amount must not reject the feed: %v", err)
	}
}

func TestTermForCycle(t *testing.T) {
	cases := map[string]int{
		"Monthly":       model.TermMonthly,
		"monthly":       model.TermMonthly,
		"Quarterly":     model.TermQuarterly,
		"Semi-Annually": model.TermSemiannual,
		"semiannually":  model.TermSemiannual,
		"Semi Annually": model.TermSemiannual,
		"Annually":      model.TermAnnual,
		"Yearly":        model.TermAnnual,
		"Biennially":    model.TermBiennial,
		"Triennially":   model.TermTriennial,
		// Unknown cycles must be reported as unmappable, never guessed.
		"Free Account": 0,
		"":             0,
		"Fortnightly":  0,
	}
	for in, want := range cases {
		if got := TermForCycle(in); got != want {
			t.Errorf("TermForCycle(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestIsActive(t *testing.T) {
	active := []string{"active", "Active", "ACTIVE", " Active "}
	for _, s := range active {
		if !IsActive(s) {
			t.Errorf("IsActive(%q) should be true", s)
		}
	}
	// Anything that is not exactly "active" is not active: an annotated
	// status must not be silently treated as one.
	inactive := []string{"", "Cancelled", "Suspended", "Terminated", "Active (Paid)", "inactive", "Fraud"}
	for _, s := range inactive {
		if IsActive(s) {
			t.Errorf("IsActive(%q) should be false", s)
		}
	}
}

func TestFetchSendsKeyAndParses(t *testing.T) {
	var gotKey, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAccept = r.Header.Get("Accept")
		fmt.Fprint(w, `{"client_id":1,"currency":"USD","services":[]}`)
	}))
	defer srv.Close()

	f, err := Fetch(context.Background(), srv.URL, "secret-key")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f.ClientID != 1 {
		t.Fatalf("bad feed: %+v", f)
	}
	if gotKey != "secret-key" || gotAccept == "" {
		t.Fatalf("headers wrong: key=%q accept=%q", gotKey, gotAccept)
	}
}

func TestFetchAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, "wrong")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("401 must map to ErrAuth, got %v", err)
	}

	// A rejected key is a distinct condition from a transport failure.
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer forbidden.Close()
	if _, err := Fetch(context.Background(), forbidden.URL, "k"); !errors.Is(err, ErrAuth) {
		t.Fatalf("403 must map to ErrAuth, got %v", err)
	}
}

// The key travels in a custom header, which Go forwards across redirects to
// any host; the client must refuse to follow one instead.
func TestFetchRefusesRedirect(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			leaked = true
		}
		fmt.Fprint(w, `{"services":[]}`)
	}))
	defer other.Close()

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer feed.Close()

	_, err := Fetch(context.Background(), feed.URL, "k")
	if err == nil {
		t.Fatal("redirected fetch must fail")
	}
	if leaked {
		t.Fatal("API key leaked to the redirect target")
	}
}

func TestFetchBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Far more than the cap; the client must stop reading.
		w.Write([]byte(strings.Repeat("x", maxBody+1024)))
	}))
	defer srv.Close()

	if _, err := Fetch(context.Background(), srv.URL, "k"); err == nil {
		t.Fatal("oversized body must be rejected")
	}
}

func TestFetchMissingKey(t *testing.T) {
	// No request should be made at all without a key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be contacted without a key")
	}))
	defer srv.Close()

	if _, err := Fetch(context.Background(), srv.URL, "  "); err == nil {
		t.Fatal("blank key must be refused")
	}
}

func TestFetchNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom\nsecond line\n")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, "k")
	if err == nil {
		t.Fatal("500 must be an error")
	}
	// The message must be a single clipped line, not a whole HTML dump.
	if strings.Count(err.Error(), "\n") != 0 {
		t.Fatalf("error must be one line, got %q", err.Error())
	}
}
