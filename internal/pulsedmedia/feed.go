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

// Package pulsedmedia reads the PulsedMedia customer API ("your own
// services"): one read-only HTTPS request authenticated with an X-API-Key
// header, returning every service on the account.
//
// Adapted from the AGPL-licensed pmss-fleet-manager project (internal/pmapi,
// same author). Only its feed half is reused; that project's SSH, qBittorrent
// and Docker components are deliberately not carried over.
package pulsedmedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
)

// FeedURL is the read-only services endpoint.
const FeedURL = "https://pulsedmedia.com/remote/pmapi-feed.php"

// maxBody caps how much of a response is read. A feed is a few hundred KB at
// most; anything larger is a wrong URL answering with something else.
const maxBody = 4 << 20

// ErrAuth means the key was rejected.
var ErrAuth = errors.New("API key rejected")

// Feed is the whole response.
type Feed struct {
	ClientID    int64     `json:"client_id"`
	GeneratedAt string    `json:"generated_at"`
	Currency    string    `json:"currency"`
	Services    []Service `json:"services"`
}

// Service is one product on the account.
type Service struct {
	ID              int64   `json:"id"`
	Product         string  `json:"product"`
	Group           string  `json:"group"`
	Domain          string  `json:"domain"`
	Status          string  `json:"status"`
	Registered      string  `json:"registered"` // YYYY-MM-DD
	NextDue         string  `json:"next_due"`   // YYYY-MM-DD
	BillingCycle    string  `json:"billing_cycle"`
	RecurringAmount Amount  `json:"recurring_amount"`
	Seedbox         *Access `json:"seedbox,omitempty"`
}

// Access is the seedbox or storage-box login block.
type Access struct {
	Username         string `json:"username"`
	LoginURL         string `json:"login_url"`
	PermanentAddress string `json:"permanent_address"`
	Usage            *Usage `json:"usage,omitempty"`
}

// Usage is the provider's own accounting of a service.
type Usage struct {
	TrackedSince   string `json:"tracked_since"`
	SentBytes      int64  `json:"sent_bytes"`
	ReceivedBytes  int64  `json:"received_bytes"`
	CPUHours       int64  `json:"cpu_hours"`
	CPUSince       string `json:"cpu_since"`
	DiskUsedBytes  int64  `json:"disk_used_bytes"`
	DiskQuotaBytes int64  `json:"disk_quota_bytes"`
	Torrents       int64  `json:"torrents"`
}

// Amount decodes a money value sent as a string ("11.94", "€11.94",
// "1,234.56", "11,94") or as a number. It never fails: one odd amount must not
// reject the whole feed and every renewal date with it, so an unparsable value
// decodes as 0.
type Amount float64

// UnmarshalJSON decodes a money value sent as a quoted string or a number,
// never failing: an unparsable value decodes as 0 (see Amount).
func (a *Amount) UnmarshalJSON(b []byte) error {
	*a = Amount(parseAmount(strings.Trim(strings.TrimSpace(string(b)), `"`)))
	return nil
}

// parseAmount parses a money string in various thousands/decimal-separator
// conventions (e.g. "1,234.56", "1.234,56", "11,94") into a float64. It
// strips stray characters and returns 0 on any parse failure or implausible
// magnitude.
func parseAmount(s string) float64 {
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '.' || r == ',' || r == '-' {
			return r
		}
		return -1
	}, s)
	// Whichever separator comes last with at most two digits after it is the
	// decimal point: "1,234.56", "1.234,56", "11,94", "1,234".
	lc, ld := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	if lc < 0 && strings.Count(s, ".") > 1 {
		s = strings.ReplaceAll(s, ".", "") // "1.234.567": dots are thousands separators
	}
	switch {
	case lc > ld && len(s)-lc-1 <= 2 && strings.Count(s, ",") == 1:
		s = strings.ReplaceAll(s[:lc], ".", "") + "." + s[lc+1:]
	default:
		s = strings.ReplaceAll(s, ",", "")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || f > 1e12 || f < -1e12 {
		return 0 // unparsable or absurd: no real invoice is anywhere near this
	}
	return f
}

// MonthsPerCycle maps a billing cycle name to months; 0 means "not recurring",
// which is also what an unrecognised cycle reports.
func MonthsPerCycle(cycle string) int {
	switch strings.ToLower(strings.TrimSpace(cycle)) {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "semi-annually", "semiannually", "semi annually":
		return 6
	case "annually", "yearly":
		return 12
	case "biennially":
		return 24
	case "triennially":
		return 36
	}
	return 0
}

// Monthly returns the recurring amount normalised to one month.
func (s Service) Monthly() float64 {
	m := MonthsPerCycle(s.BillingCycle)
	if m == 0 {
		return 0
	}
	return float64(s.RecurringAmount) / float64(m)
}

// Username returns the service's shell/web user, if it has one.
func (s Service) Username() string {
	if s.Seedbox == nil {
		return ""
	}
	return s.Seedbox.Username
}

// TermForCycle maps a billing cycle to idlerthing's pricing term, or 0 when
// the cycle is not one idlerthing can represent.
//
// Callers must treat 0 as "cannot map" and write neither the price nor the
// term: a price stored without its cycle reports a wrong monthly cost, and
// guessing a cycle would invent a number.
func TermForCycle(cycle string) int {
	switch MonthsPerCycle(cycle) {
	case 1:
		return model.TermMonthly
	case 3:
		return model.TermQuarterly
	case 6:
		return model.TermSemiannual
	case 12:
		return model.TermAnnual
	case 24:
		return model.TermBiennial
	case 36:
		return model.TermTriennial
	}
	return 0
}

// IsActive reports whether a feed status counts as active. Only an exact
// "active" (case-insensitive) does: an annotated status such as
// "Active (Paid)" is not treated as one rather than guessed at.
func IsActive(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "active")
}

// Parse decodes a feed body.
func Parse(body []byte) (*Feed, error) {
	var f Feed
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse feed: %w (%s)", err, firstLine(body))
	}
	return &f, nil
}

// Fetch downloads and parses the feed with the given key. An empty url uses
// FeedURL; callers pass an override so tests can point at a mock server.
func Fetch(ctx context.Context, url, key string) (*Feed, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("no API key configured")
	}
	if url == "" {
		url = FeedURL
	}
	// The key travels in a custom header, which Go (unlike Authorization)
	// forwards unchanged across redirects to any host or scheme. The feed has
	// one fixed URL, so a redirect is refused rather than followed.
	hc := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("feed redirected to %s://%s; refusing to send the API key there",
				req.URL.Scheme, req.URL.Host)
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "idlerthing")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("feed response larger than %d bytes", maxBody)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(body))
	}
	return Parse(body)
}

// firstLine keeps a server's error body to one clipped line so it cannot
// become an unbounded blob in an alert or a log.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
