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
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
	"idlerthing/internal/pulsedmedia"
)

// fleetStaleAfter is how old a cache may get before the view says so.
const fleetStaleAfter = 6 * time.Hour

// fleetBox is one seedbox row: durable values from the inventory (which the
// sync has already reconciled) and live figures from the feed cache. The view
// performs no precedence logic of its own — one rule, one place.
type fleetBox struct {
	Hostname  string
	Product   string
	Status    string
	Cancelled bool

	DiskUsedMB  int64
	DiskQuotaMB int64
	HasQuota    bool
	DiskPct     int
	DiskMeter   *meterView

	Sent         string
	Received     string
	SentMB       int64
	ReceivedMB   int64
	CPUHours     int64
	Torrents     int64
	TrackedSince string
	CPUSince     string

	NextDue    string
	Monthly    string
	MonthlyUSD float64
}

// fleetTotals are the headline tiles. They count active services only: a
// cancelled service's price is not money being spent.
type fleetTotals struct {
	Boxes         int
	Cancelled     int // listed on the page but excluded from every figure here
	StorageMB     int64
	StorageUsedMB int64
	SentMB        int64
	ReceivedMB    int64
	Torrents      int64
	CPUHours      int64
	MonthlyUSD    float64
	NextDue       string
	NextDueBox    string
}

// fleetView is the template payload.
type fleetView struct {
	Boxes         []fleetBox
	Totals        fleetTotals
	ShowCancelled bool
	SyncedAt      string
	Stale         bool
	Configured    bool   // at least one provider holds an API key
	RatesNote     string // non-empty when conversions are unavailable
}

// handleSeedboxFleet renders GET /seedboxes/fleet.
//
// The page renders from the cache only: a slow or failing feed never blocks a
// page load, and the age of the data is reported rather than hidden.
func (s *Server) handleSeedboxFleet(w http.ResponseWriter, r *http.Request) {
	view, err := s.buildFleetView(r)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "PulsedMedia fleet", "seedboxes")
	data.Data = view
	s.render(w, r, "seedbox_fleet", data)
}

// buildFleetView assembles the fleet from the cache and the inventory.
func (s *Server) buildFleetView(r *http.Request) (fleetView, error) {
	view := fleetView{ShowCancelled: isCancelledRequest(r)}

	providers, err := s.catalogs.ProvidersWithKey(r.Context())
	if err != nil {
		return view, err
	}
	view.Configured = len(providers) > 0

	// Rates can go stale without the page saying so: the tiles would quietly
	// mix currencies otherwise (same note the dashboard shows).
	var newest time.Time
	rates, ratesOK := s.rates.Get(r.Context())
	if !ratesOK {
		view.RatesNote = "exchange rates unavailable — showing USD only"
	}

	for _, p := range providers {
		services, err := (&model.PMStore{DB: s.db}).List(r.Context(), p.ID)
		if err != nil {
			return view, err
		}
		for _, svc := range services {
			if t, err := time.Parse(time.RFC3339, svc.SyncedAt); err == nil && t.After(newest) {
				newest = t
			}
		}
		boxes, err := s.fleetRows(r, p.ID, services, rates)
		if err != nil {
			return view, err
		}
		view.Boxes = append(view.Boxes, boxes...)
	}

	if !newest.IsZero() {
		view.SyncedAt = newest.Format("2006-01-02 15:04")
		view.Stale = time.Since(newest) > fleetStaleAfter
	}
	view.Totals = fleetTiles(view.Boxes)
	return view, nil
}

// fleetRows joins one provider's linked seedboxes to its cached services.
func (s *Server) fleetRows(r *http.Request, providerID int64, services []model.PMService, rates map[string]float64) ([]fleetBox, error) {
	byService := make(map[int64]model.PMService, len(services))
	for _, svc := range services {
		byService[svc.ServiceID] = svc
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT s.hostname, s.pm_service_id, s.disk_as_mb,
			COALESCE(pr.currency, ''), pr.price, pr.term, pr.next_due_date
		FROM seedboxes s
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = ? AND pr.active = 1
		WHERE s.provider_id = ? AND s.pm_service_id IS NOT NULL
		ORDER BY s.hostname COLLATE NOCASE`, model.ServiceSeedbox, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []fleetBox
	for rows.Next() {
		var hostname string
		var serviceID int64
		var diskMB sql.NullInt64
		var currency, due sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		if err := rows.Scan(&hostname, &serviceID, &diskMB, &currency, &price, &term, &due); err != nil {
			return nil, err
		}
		svc, ok := byService[serviceID]
		if !ok {
			continue // the feed no longer lists it; not a fleet row
		}
		// Cancelled and suspended services are hidden unless asked for. They
		// are excluded from the tiles either way.
		if !pulsedmedia.IsActive(svc.Status) && !isCancelledRequest(r) {
			continue
		}
		monthlyUSD, _ := pricing.MonthlyUSDRaw(&model.Pricing{
			Currency: currency.String,
			Price:    price.Float64,
			Term:     int(term.Int64),
		}, rates)
		out = append(out, buildFleetBox(hostname, svc, diskMB, due.String, monthlyUSD))
	}
	return out, rows.Err()
}

// buildFleetBox renders one box from its cached service and inventory values.
func buildFleetBox(hostname string, svc model.PMService, diskMB sql.NullInt64, due string, monthlyUSD float64) fleetBox {
	box := fleetBox{
		Hostname:     hostname,
		Product:      svc.Product,
		Status:       svc.Status,
		Cancelled:    !pulsedmedia.IsActive(svc.Status),
		DiskUsedMB:   svc.DiskUsedBytes / (1024 * 1024),
		Sent:         fmtMB(svc.SentBytes / (1024 * 1024)),
		Received:     fmtMB(svc.ReceivedBytes / (1024 * 1024)),
		SentMB:       svc.SentBytes / (1024 * 1024),
		ReceivedMB:   svc.ReceivedBytes / (1024 * 1024),
		CPUHours:     svc.CPUHours,
		Torrents:     svc.Torrents,
		TrackedSince: svc.TrackedSince,
		CPUSince:     svc.CPUSince,
		MonthlyUSD:   monthlyUSD,
	}

	// The inventory's due date wins when set: it is what the rest of the app
	// shows, and the sync already applied the precedence rule to it.
	box.NextDue = due
	if box.NextDue == "" {
		box.NextDue = svc.NextDue
	}
	if diskMB.Valid {
		box.DiskQuotaMB = diskMB.Int64
	}
	if box.DiskQuotaMB > 0 {
		box.HasQuota = true
		box.DiskPct = int(box.DiskUsedMB * 100 / box.DiskQuotaMB)
		box.DiskMeter = meter(float64(box.DiskUsedMB) * 100 / float64(box.DiskQuotaMB))
	}
	if monthlyUSD > 0 {
		box.Monthly = "$" + strconv.FormatFloat(monthlyUSD, 'f', 2, 64)
	}
	return box
}

// fleetTiles sums the headline figures over active boxes.
func fleetTiles(boxes []fleetBox) fleetTotals {
	var t fleetTotals
	for _, b := range boxes {
		if b.Cancelled {
			t.Cancelled++
			continue
		}
		t.Boxes++
		t.StorageMB += b.DiskQuotaMB
		t.StorageUsedMB += b.DiskUsedMB
		t.SentMB += b.SentMB
		t.ReceivedMB += b.ReceivedMB
		t.Torrents += b.Torrents
		t.CPUHours += b.CPUHours
		t.MonthlyUSD += b.MonthlyUSD
		if b.NextDue != "" && (t.NextDue == "" || b.NextDue < t.NextDue) {
			t.NextDue = b.NextDue
			t.NextDueBox = b.Hostname
		}
	}
	return t
}

// isCancelledRequest reports whether the request carries ?cancelled=1.
func isCancelledRequest(r *http.Request) bool { return r.URL.Query().Get("cancelled") == "1" }

// handleSeedboxFleetSync handles POST /seedboxes/fleet/sync: fetch each keyed
// provider's feed, then link and reconcile it. Sync does both in one
// transaction, so a service discovered for the first time is reconciled on
// this press rather than the next one.
//
// A failure is reported through a flash rather than an error page: the cached
// view still works, and the last good data is what the user should see. The
// per-provider reason goes to the journal instead of the flash — a transport
// error is not actionable in a banner, and it is the only record of WHICH
// provider failed.
func (s *Server) handleSeedboxFleetSync(w http.ResponseWriter, r *http.Request) {
	providers, err := s.catalogs.ProvidersWithKey(r.Context())
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	s.pmSyncMu.Lock()
	if s.pmSyncing {
		s.pmSyncMu.Unlock()
		s.setFlash(w, r, "err", "A sync is already running.")
		http.Redirect(w, r, routeSeedboxFleet, http.StatusSeeOther)
		return
	}
	s.pmSyncing = true
	s.pmSyncMu.Unlock()
	defer func() {
		s.pmSyncMu.Lock()
		s.pmSyncing = false
		s.pmSyncMu.Unlock()
	}()

	// The r context is deliberately NOT used for the fetches below.
	// http.Server sets WriteTimeout to 120s and cancels the request context
	// when a write times out, so one slow provider could abort the whole
	// batch mid-flight. Each stage gets its own deadline; the page redirect
	// still follows the request's lifetime.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
		syncBudget(len(providers)))
	defer cancel()

	feed, reconciled, failed, authFailed := 0, 0, 0, 0
	for _, p := range providers {
		key, err := s.catalogs.ProviderKey(ctx, p.ID)
		if err != nil {
			failed++
			slog.Error("provider key read failed", "provider", p.Name, "id", p.ID, "err", err)
			continue
		}
		if key == "" {
			// The query above selected on pm_api_key <> '', so an empty key
			// here means it was cleared while the sync was running.
			slog.Warn("provider lost its key mid-sync", "provider", p.Name, "id", p.ID)
			continue
		}
		res, err := pulsedmedia.Sync(ctx, s.db, p.ID, key, s.pmapiURL, time.Now())
		if err != nil {
			failed++
			if errors.Is(err, pulsedmedia.ErrAuth) {
				authFailed++
			}
			slog.Error("provider sync failed", "provider", p.Name, "id", p.ID, "err", err)
			continue
		}
		// Sync links newly discovered boxes and reconciles them in the same
		// transaction, so there is no separate pass to run here.
		feed += res.Synced
		reconciled += res.Matched
		if res.Synced > 0 && res.Matched == 0 {
			slog.Info("provider feed linked to nothing in the inventory",
				"provider", p.Name, "services", res.Synced)
		}
	}

	s.touchDashboard()
	kind, msg := "ok", ""
	switch {
	case failed == len(providers) && len(providers) > 0:
		kind, msg = "err", "Every provider failed to sync — the journal names the reason."
	case failed > 0:
		kind, msg = "err", "Sync failed for "+strconv.Itoa(failed)+" of "+
			strconv.Itoa(len(providers))+" providers — showing the last good data."
	case feed == 0:
		msg = "Sync finished; the feed listed no services."
	case reconciled == 0:
		// Cached but unlinked: without this the page just looks empty after a
		// "successful" sync.
		msg = "Fetched " + strconv.Itoa(feed) +
			" service(s); none are linked to a seedbox yet — link them on the seedbox form."
	default:
		msg = "Synced " + strconv.Itoa(feed) + " service(s), reconciled " +
			strconv.Itoa(reconciled) + " linked box(es)."
	}
	if authFailed > 0 {
		kind = "err"
		msg += " At least one API key was rejected — re-enter it under Catalogs → Providers."
	}
	s.setFlash(w, r, kind, msg)
	http.Redirect(w, r, routeSeedboxFleet, http.StatusSeeOther)
}

// syncBudget sizes the whole-batch deadline: Fetch allows 30s per provider,
// plus room for the reconcile and link passes, capped so a hung endpoint cannot
// keep the singleflight slot (and therefore every later click) busy forever.
func syncBudget(providers int) time.Duration {
	const perProvider = 45 * time.Second
	const max = 5 * time.Minute
	if d := time.Duration(providers) * perProvider; d < max {
		return d
	}
	return max
}
