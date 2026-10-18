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
	"database/sql"
	"net/http"
	"strconv"
	"sync"
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

	Sent       string
	Received   string
	SentMB     int64
	ReceivedMB int64
	CPUHours   int64
	Torrents   int64

	NextDue    string
	Monthly    string
	MonthlyUSD float64
}

// fleetTotals are the headline tiles. They count active services only: a
// cancelled service's price is not money being spent.
type fleetTotals struct {
	Boxes         int
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
	Configured    bool // at least one provider holds an API key
}

// pmSyncFlight serialises syncs so a burst of clicks cannot launch several
// fetches at once (the provider rate-limits by key).
var (
	pmSyncMu     sync.Mutex
	pmSyncFlight bool
)

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

	providers, err := s.catalogs.ProviderIDsWithKey(r.Context())
	if err != nil {
		return view, err
	}
	view.Configured = len(providers) > 0

	rates, _ := s.rates.Get(r.Context())
	var newest time.Time

	for _, providerID := range providers {
		services, err := (&model.PMStore{DB: s.db}).List(r.Context(), providerID)
		if err != nil {
			return view, err
		}
		for _, svc := range services {
			if t, err := time.Parse(time.RFC3339, svc.SyncedAt); err == nil && t.After(newest) {
				newest = t
			}
		}
		boxes, err := s.fleetRows(r, providerID, services, rates)
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
		risk, _ := pricing.MonthlyUSD(&model.Pricing{
			Currency: currency.String,
			Price:    price.Float64,
			Term:     int(term.Int64),
		}, rates)
		out = append(out, buildFleetBox(hostname, svc, diskMB, due.String, risk))
	}
	return out, rows.Err()
}

// buildFleetBox renders one box from its cached service and inventory values.
func buildFleetBox(hostname string, svc model.PMService, diskMB sql.NullInt64, due string, monthlyUSD float64) fleetBox {
	box := fleetBox{
		Hostname:   hostname,
		Product:    svc.Product,
		Status:     svc.Status,
		Cancelled:  !pulsedmedia.IsActive(svc.Status),
		DiskUsedMB: svc.DiskUsedBytes / (1024 * 1024),
		Sent:       fmtMB(svc.SentBytes / (1024 * 1024)),
		Received:   fmtMB(svc.ReceivedBytes / (1024 * 1024)),
		SentMB:     svc.SentBytes / (1024 * 1024),
		ReceivedMB: svc.ReceivedBytes / (1024 * 1024),
		CPUHours:   svc.CPUHours,
		Torrents:   svc.Torrents,
		MonthlyUSD: monthlyUSD,
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

func isCancelledRequest(r *http.Request) bool { return r.URL.Query().Get("cancelled") == "1" }

// handleSeedboxFleetSync handles POST /seedboxes/fleet/sync: fetch each keyed
// provider's feed, reconcile it, then re-link any new seedboxes.
//
// A failure is reported through a flash rather than an error page: the cached
// view still works, and the last good data is what the user should see.
func (s *Server) handleSeedboxFleetSync(w http.ResponseWriter, r *http.Request) {
	providers, err := s.catalogs.ProviderIDsWithKey(r.Context())
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	pmSyncMu.Lock()
	if pmSyncFlight {
		pmSyncMu.Unlock()
		s.setFlash(w, r, "err", "A sync is already running.")
		http.Redirect(w, r, routeSeedboxFleet, http.StatusSeeOther)
		return
	}
	pmSyncFlight = true
	pmSyncMu.Unlock()
	defer func() {
		pmSyncMu.Lock()
		pmSyncFlight = false
		pmSyncMu.Unlock()
	}()

	synced, failed := 0, 0
	for _, providerID := range providers {
		key, err := s.catalogs.ProviderKey(r.Context(), providerID)
		if err != nil || key == "" {
			continue
		}
		res, err := pulsedmedia.Sync(r.Context(), s.db, providerID, key, s.pmapiURL, time.Now())
		if err != nil {
			// Auth failures are worth distinguishing: the key needs fixing,
			// and retrying will not help.
			failed++
			continue
		}
		// Sync now links and reconciles new boxes in the same transaction.
		synced += res.Synced
	}

	s.touchDashboard()
	switch {
	case failed > 0:
		s.setFlash(w, r, "err", "Sync failed for one or more providers — showing the last good data.")
	case synced == 0:
		s.setFlash(w, r, "ok", "Sync finished; the feed listed no services.")
	default:
		s.setFlash(w, r, "ok", "Synced "+strconv.Itoa(synced)+" service(s).")
	}
	http.Redirect(w, r, routeSeedboxFleet, http.StatusSeeOther)
}
