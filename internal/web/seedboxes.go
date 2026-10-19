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
	"errors"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// seedboxSection builds the seedboxes list/detail/delete section.
func (s *Server) seedboxSection() *section {
	st := &model.SeedboxStore{DB: s.db}
	return &section{
		Base:        routeSeedboxes,
		Kind:        "seedboxes",
		Title:       "SeedBoxes",
		Singular:    "Seedbox",
		ServiceType: model.ServiceSeedbox,
		AddLabel:    "＋ Add seedbox",
		SearchHint:  "Search hostname, title, provider…",
		EmptyTitle:  "No seedboxes yet",
		EmptySub:    "Add your first one to start tracking.",
		DefaultSort: "hostname",
		Columns: []listColumn{
			{Key: "title", Label: "Title", Sortable: true},
			{Key: "hostname", Label: "Hostname", Sortable: true},
			{Key: "type", Label: "Type", Sortable: true},
			{Key: "port", Label: "Port", Sortable: true},
			{Key: "disk", Label: "Disk", Sortable: true},
			{Key: "bw", Label: "BW", Sortable: true},
			{Key: "location", Label: "Location", Sortable: true},
			{Key: "provider", Label: "Provider", Sortable: true},
			{Key: "price", Label: "Price", Sortable: true},
			{Key: "due", Label: "Due", Sortable: true},
		},
		List: func(r *http.Request, opts model.ListOptions) ([]listRow, error) {
			items, err := st.List(r.Context(), opts)
			if err != nil {
				return nil, err
			}
			dueSoon := s.dueSoonDays(r)
			var rows []listRow
			for _, it := range items {
				id := strconv.FormatInt(it.ID, 10)
				dot := "ok"
				if !it.Active {
					dot = "off"
				}
				title := it.Title.String
				if title == "" {
					title = "—"
				}
				rows = append(rows, listRow{
					Link:          routeSeedboxes + "/" + id,
					EditURL:       routeSeedboxes + "/" + id + "/edit",
					DeleteURL:     routeSeedboxes + "/" + id + "/delete",
					DeleteConfirm: "Delete " + it.Hostname + "?",
					Cells: []listCell{
						{Main: title, Dot: dot, Link: routeSeedboxes + "/" + id},
						{Main: it.Hostname, Class: "mono"},
						{Main: dash(it.SeedBoxType.String), Badge: it.SeedBoxType.Valid},
						{Main: nullMbps(it.PortSpeed), Class: "mono"},
						{Main: fmtNullMB(it.DiskAsMB), Class: "mono"},
						bwCell(it.BandwidthAsMB),
						{Main: dash(it.LocationName)},
						{Main: dash(it.ProviderName)},
						pricingCell(it.Pricing),
						dueCell(it.Pricing, dueSoon),
					},
				})
			}
			return rows, nil
		},
		Counts: st.StatusCounts,
		Cards: func(r *http.Request, active, inactive int) []statCard {
			monthly, yearly := s.costPairUSDFor(r, model.ServiceSeedbox)
			return []statCard{
				{Label: "Total", Value: strconv.Itoa(active + inactive)},
				{Label: "Active", Value: strconv.Itoa(active)},
				{Label: "Monthly cost", Value: monthly},
				{Label: "Yearly cost", Value: yearly},
			}
		}, Delete: st.Delete,
		Detail: func(r *http.Request, id int64) (*detailView, error) {
			b, pricing, err := st.Get(r.Context(), id)
			if err != nil {
				return nil, err
			}
			badges := []detailBadge{}
			if b.Active {
				badges = append(badges, detailBadge{Label: "Active", Class: "badge-ok"})
			} else {
				badges = append(badges, detailBadge{Label: "Inactive", Class: "badge-off"})
			}
			if b.WasPromo {
				badges = append(badges, detailBadge{Label: "Promo", Class: "badge-warn"})
			}
			if b.ShowPublic {
				badges = append(badges, detailBadge{Label: "Public"})
			}
			kvs := func(k string, v sql.NullString) kvPair {
				if !v.Valid {
					return kvPair{K: k, V: "—"}
				}
				return kvPair{K: k, V: v.String}
			}
			kvn := func(k string, n sql.NullInt64, suffix string) kvPair {
				if !n.Valid {
					return kvPair{K: k, V: "—"}
				}
				return kvPair{K: k, V: strconv.FormatInt(n.Int64, 10) + suffix, Mono: true}
			}
			title := b.Hostname
			if b.Title.Valid && b.Title.String != "" {
				title = b.Title.String
			}
			sid := strconv.FormatInt(b.ID, 10)
			pollURL := ""
			if b.SSHPollEnabled && b.Active && (b.SSHHostMetrics || b.SSHMemoryMetrics) {
				pollURL = routeSeedboxes + "/" + sid + "/poll"
			}
			sshStatus := "Off"
			if b.SSHPollEnabled {
				sshStatus = "Enabled (manual)"
			}
			return &detailView{
				Title:         title,
				Mono:          true,
				Badges:        badges,
				EditURL:       routeSeedboxes + "/" + sid + "/edit",
				DeleteURL:     routeSeedboxes + "/" + sid + "/delete",
				DeleteConfirm: "Delete " + b.Hostname + "?",
				PollURL:       pollURL,
				Cards: []infoCard{
					{Title: "Service", Pairs: []kvPair{
						kvs("Title", b.Title),
						{K: "Hostname", V: b.Hostname, Mono: true},
						kvs("Type", b.SeedBoxType),
					}},
					{Title: "Resources", Pairs: []kvPair{
						kvn("Port speed", b.PortSpeed, " mbps"),
						{K: "Disk", V: fmtNullMB(b.DiskAsMB), Mono: true},
						{K: "Bandwidth", V: bwDisplay(b.BandwidthAsMB), Mono: true},
					}},
					{Title: "Classification", Pairs: []kvPair{
						{K: "Provider", V: dash(s.catalogName(r, "providers", b.ProviderID))},
						{K: "Location", V: dash(s.catalogName(r, "locations", b.LocationID))},
						kvs("Owned since", b.OwnedSince),
					}},
					{Title: "Pricing", Pairs: pricingPairs(pricing), Empty: "No pricing attached."},
					{Title: "SSH polling", Pairs: []kvPair{
						{K: "Status", V: sshStatus},
						{K: "SSH user", V: dash(b.SSHUser)},
					}},
				},
			}, nil
		},
	}
}

// nullMbps renders a nullable port speed for display ("—" when unset).
func nullMbps(n sql.NullInt64) string {
	if !n.Valid {
		return "—"
	}
	return strconv.FormatInt(n.Int64, 10) + " Mbps"
}

// seedboxFormView is the template payload for the seedbox form.
type seedboxFormView struct {
	Action     string
	CancelURL  string
	IsEdit     bool
	Seedbox    *model.Seedbox
	Pricing    *model.Pricing
	Providers  []model.CatalogItem
	Locations  []model.CatalogItem
	Currencies []string
	// DefaultCurrency pre-fills the currency select when no pricing exists.
	DefaultCurrency string
	Errors          map[string]string

	// FeedServices are the cached PulsedMedia services for this seedbox's
	// provider, empty when the provider has no API key. A linked-but-missing
	// service (a stale id) is shown separately so a save cannot drop it.
	FeedServices []model.PMService
	StaleFeed    bool
}

// handleSeedboxNew renders GET /seedboxes/new.
func (s *Server) handleSeedboxNew(w http.ResponseWriter, r *http.Request) {
	s.renderSeedboxForm(w, r, seedboxFormView{
		Action:  routeSeedboxes,
		Seedbox: &model.Seedbox{Active: true},
	})
}

// feedServicesFor returns the cached feed services for a seedbox's provider,
// and whether its stored link points at a service the cache no longer holds.
// A stale link is reported rather than hidden: the form must show it so an
// unrelated save does not silently drop it.
func (s *Server) feedServicesFor(r *http.Request, b *model.Seedbox) ([]model.PMService, bool) {
	if b == nil || !b.ProviderID.Valid {
		return nil, false
	}
	st := &model.CatalogStore{DB: s.db}
	hasKey, err := st.HasProviderKey(r.Context(), b.ProviderID.Int64)
	if err != nil || !hasKey {
		return nil, false
	}
	services, err := (&model.PMStore{DB: s.db}).List(r.Context(), b.ProviderID.Int64)
	if err != nil {
		return nil, false
	}
	if !b.PMServiceID.Valid || len(services) == 0 {
		return services, false
	}
	for _, svc := range services {
		if svc.ServiceID == b.PMServiceID.Int64 {
			return services, false
		}
	}
	return services, true
}

// handleSeedboxEdit renders GET /seedboxes/{id}/edit.
func (s *Server) handleSeedboxEdit(w http.ResponseWriter, r *http.Request) {
	st := &model.SeedboxStore{DB: s.db}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, pricing, err := st.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.renderSeedboxForm(w, r, seedboxFormView{
		Action:  routeSeedboxes + "/" + strconv.FormatInt(id, 10) + "/update",
		IsEdit:  true,
		Seedbox: b,
		Pricing: pricing,
	})
}

// renderSeedboxForm renders the shared new/edit form.
func (s *Server) renderSeedboxForm(w http.ResponseWriter, r *http.Request, view seedboxFormView) {
	if view.Providers == nil {
		view.Providers, _ = s.catalogs.List(r.Context(), model.Catalogs["providers"])
		view.Locations, _ = s.catalogs.List(r.Context(), model.Catalogs["locations"])
	}
	view.Currencies = currencies
	view.DefaultCurrency = s.defaultCurrency(r)
	// Feed services are offered only for a provider that holds a key: without
	// one there is no feed to link to.
	view.FeedServices, view.StaleFeed = s.feedServicesFor(r, view.Seedbox)
	title := "Add seedbox"
	view.CancelURL = routeSeedboxes
	if view.IsEdit {
		title = "Edit " + view.Seedbox.Hostname
		view.CancelURL = routeSeedboxes + "/" + strconv.FormatInt(view.Seedbox.ID, 10)
	}
	data := s.newPageData(w, r, title, "seedboxes")
	data.Data = view
	s.render(w, r, "seedbox_form", data)
}

// handleSeedboxCreate handles POST /seedboxes.
func (s *Server) handleSeedboxCreate(w http.ResponseWriter, r *http.Request) {
	b, pricing, errs := parseSeedboxForm(r)
	applySeedboxFeedLink(r, b, sql.NullInt64{})
	if len(errs) > 0 {
		s.renderSeedboxForm(w, r, seedboxFormView{
			Action: routeSeedboxes, Seedbox: b, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.SeedboxStore{DB: s.db}
	id, err := st.Create(r.Context(), b, pricing)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", b.Hostname+" added.")
	http.Redirect(w, r, routeSeedboxes+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleSeedboxUpdate handles POST /seedboxes/{id}/update.
func (s *Server) handleSeedboxUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st := &model.SeedboxStore{DB: s.db}

	// The stored link is read before parsing so a post that never carried
	// the field keeps it rather than clearing it.
	var stored sql.NullInt64
	if cur, _, err := st.Get(r.Context(), id); err == nil && cur != nil {
		stored = cur.PMServiceID
	}

	b, pricing, errs := parseSeedboxForm(r)
	applySeedboxFeedLink(r, b, stored)
	b.ID = id
	if len(errs) > 0 {
		s.renderSeedboxForm(w, r, seedboxFormView{
			Action: routeSeedboxes + "/" + strconv.FormatInt(id, 10) + "/update",
			IsEdit: true, Seedbox: b, Pricing: pricing, Errors: errs,
		})
		return
	}
	if err := st.Update(r.Context(), b, pricing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", b.Hostname+" saved.")
	http.Redirect(w, r, routeSeedboxes+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseSeedboxForm parses + validates the seedbox form.
//
// The feed link is three-valued: absent means "unchanged" (a client or a
// partial post that never carried the field must not silently unlink),
// empty means "unlink", and a value links.
func parseSeedboxForm(r *http.Request) (*model.Seedbox, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	b := &model.Seedbox{
		Title:            nullStrFormValue(r, "title"),
		Hostname:         strings.TrimSpace(r.FormValue("hostname")),
		SeedBoxType:      nullStrFormValue(r, "seed_box_type"),
		ProviderID:       nullIntFormValue(r, "provider_id"),
		LocationID:       nullIntFormValue(r, "location_id"),
		Active:           r.FormValue("active") != "",
		ShowPublic:       r.FormValue("show_public") != "",
		WasPromo:         r.FormValue("was_promo") != "",
		SSHPollEnabled:   r.FormValue("ssh_poll_enabled") != "",
		SSHUser:          strings.TrimSpace(r.FormValue("ssh_user")),
		SSHHostMetrics:   r.FormValue("ssh_host_metrics") != "",
		SSHMemoryMetrics: r.FormValue("ssh_memory_metrics") != "",
	}
	if b.Hostname == "" {
		errs["hostname"] = "Hostname is required."
	}
	if (b.SSHHostMetrics || b.SSHMemoryMetrics) && !b.SSHPollEnabled {
		errs["ssh_poll_enabled"] = "Enable SSH polling before selecting metrics."
	}
	if b.SSHPollEnabled && b.SSHUser == "" {
		errs["ssh_user"] = "SSH username is required when polling is enabled."
	}
	if b.SSHPollEnabled && !validSSHTarget(b.Hostname) {
		errs["hostname"] = "SSH polling requires a DNS hostname, not an IP or local address."
	}
	if len(b.SSHUser) > 64 {
		errs["ssh_user"] = "SSH username is too long."
	}
	if strings.HasPrefix(b.SSHUser, "-") {
		errs["ssh_user"] = "SSH username cannot start with -."
	}
	for _, c := range b.SSHUser {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-", c) {
			errs["ssh_user"] = "Use letters, numbers, _, . or - in SSH username."
			break
		}
	}
	b.PortSpeed = checkedInt(r, errs, "port_speed", 0, 1<<20)
	b.DiskAsMB = sizeFormValue(r, errs, "disk_as_mb", 1<<30)
	b.BandwidthAsMB = bandwidthFormValue(r, errs, "bandwidth_as_mb", 1<<30)
	b.OwnedSince = dateFormValue(r, errs, "owned_since")
	return b, parsePricingForm(r, errs), errs
}

// applySeedboxFeedLink sets the feed link from the form, distinguishing an
// absent field (keep the stored link) from an empty one (clear it).
func applySeedboxFeedLink(r *http.Request, b *model.Seedbox, stored sql.NullInt64) {
	if _, present := r.Form["pm_service_id"]; !present {
		b.PMServiceID = stored
		return
	}
	v := strings.TrimSpace(r.FormValue("pm_service_id"))
	if v == "" {
		b.PMServiceID = sql.NullInt64{}
		return
	}
	if id, err := strconv.ParseInt(v, 10, 64); err == nil {
		b.PMServiceID = sql.NullInt64{Int64: id, Valid: true}
		return
	}
	b.PMServiceID = stored
}
