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

// catalogView is the template payload for catalog list pages.
type catalogView struct {
	Kind       string // url kind, e.g. "providers"
	Title      string
	Items      []model.CatalogItem
	Error      string        // inline error shown above the table (htmx swaps)
	Counts     map[int64]int // usage counts (labels only)
	ShowCounts bool

	// Providers only: whether each row holds a PulsedMedia API key. The key
	// itself is never read into the view, so it cannot reach the template.
	ShowKeys bool
	HasKey   map[int64]bool
	AutoAdd  map[int64]bool
}

// newCatalogView builds the list payload, including the per-row hints that
// are never the secret itself: usage counts for labels, "a key is set" for
// providers.
func (s *Server) newCatalogView(r *http.Request, kindStr string, kind model.CatalogKind, items []model.CatalogItem, errMsg string) catalogView {
	view := catalogView{Kind: kindStr, Title: kind.Title, Items: items, Error: errMsg}
	if kindStr == "labels" {
		view.ShowCounts = true
		view.Counts = map[int64]int{}
		counts, _ := (&model.LabelStore{DB: s.db}).AllWithCounts(r.Context())
		for _, c := range counts {
			view.Counts[c.ID] = c.Used
		}
	}
	if kindStr == "providers" {
		view.ShowKeys = true
		view.HasKey = map[int64]bool{}
		view.AutoAdd = map[int64]bool{}
		rows, err := s.db.QueryContext(r.Context(), "SELECT id FROM providers WHERE pm_auto_add = 1")
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					view.AutoAdd[id] = true
				}
			}
			rows.Close()
		}
		providers, err := (&model.CatalogStore{DB: s.db}).ProvidersWithKey(r.Context())
		if err == nil {
			for _, p := range providers {
				view.HasKey[p.ID] = true
			}
		}
	}
	return view
}

// catalogKind resolves the {kind} path value to a catalog definition.
func catalogKind(r *http.Request) (string, model.CatalogKind, bool) {
	kindStr := r.PathValue("kind")
	kind, ok := model.Catalogs[kindStr]
	return kindStr, kind, ok
}

// handleCatalogList renders GET /catalogs/{kind}.
func (s *Server) handleCatalogList(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	items, err := s.catalogs.List(r.Context(), kind)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	view := s.newCatalogView(r, kindStr, kind, items, "")
	data := s.newPageData(w, r, kind.Title, kindStr)
	data.Data = view
	s.render(w, r, "catalogs", data)
}

// handleCatalogCreate handles POST /catalogs/{kind}.
func (s *Server) handleCatalogCreate(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))

	errMsg := ""
	if name == "" {
		errMsg = "Name is required."
	} else if _, err := s.catalogs.Create(r.Context(), kind, name); err != nil {
		if errors.Is(err, model.ErrConflict) {
			errMsg = "That name already exists."
		} else {
			errMsg = "Save failed."
		}
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// handleCatalogUpdate handles POST /catalogs/{kind}/{id}/update.
func (s *Server) handleCatalogUpdate(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))

	errMsg := ""
	if name == "" {
		errMsg = "Name is required."
	} else if err := s.catalogs.Update(r.Context(), kind, id, name); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			errMsg = "That entry no longer exists — it may have been deleted."
		case errors.Is(err, model.ErrConflict):
			errMsg = "That name already exists."
		default:
			errMsg = "Save failed."
		}
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// handleCatalogDelete handles POST /catalogs/{kind}/{id}/delete. Deleting an
// entry still referenced by services is refused with an inline message.
func (s *Server) handleCatalogDelete(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	errMsg := ""
	// UsageCount up front so the refusal message has the count without
	// re-running it after the failed delete.
	n, _ := s.catalogs.UsageCount(r.Context(), kind, id)
	if err := s.catalogs.Delete(r.Context(), kind, id); err != nil {
		if errors.Is(err, model.ErrInUse) {
			errMsg = "In use by " + strconv.Itoa(n) + " service(s) — remove those first."
		} else {
			errMsg = "Delete failed."
		}
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// handleProviderKey handles POST /catalogs/providers/{id}/key — the write-only
// PulsedMedia API key.
//
// A blank field keeps the stored key, so a save that never touched the field
// cannot erase it; removing a key is an explicit action.
func (s *Server) handleProviderKey(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok || kindStr != "providers" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	st := &model.CatalogStore{DB: s.db}
	errMsg := ""
	switch key := strings.TrimSpace(r.FormValue("pm_api_key")); {
	case r.FormValue("clear_key") != "":
		err = st.SetProviderKey(r.Context(), id, "")
	case key == "":
		err = nil // untouched: keep the stored key
	default:
		err = st.SetProviderKey(r.Context(), id, key)
	}

	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		errMsg = "That entry no longer exists — it may have been deleted."
	default:
		errMsg = "Save failed."
	}
	if errMsg == "" {
		_, err = s.db.ExecContext(r.Context(), "UPDATE providers SET pm_auto_add = ? WHERE id = ?", boolToIntWeb(r.FormValue("pm_auto_add") != ""), id)
		if err != nil {
			errMsg = "Save failed."
		}
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// respondCatalogMutation re-renders the list partial for htmx requests, or
// redirects back to the list (with flash on error) for plain posts.
func (s *Server) respondCatalogMutation(w http.ResponseWriter, r *http.Request, kindStr string, kind model.CatalogKind, errMsg string) {
	if r.Header.Get("HX-Request") != "true" {
		if errMsg != "" {
			s.setFlash(w, r, "err", errMsg)
		} else {
			s.touchDashboard()
			s.setFlash(w, r, "ok", "Saved.")
		}
		http.Redirect(w, r, "/catalogs/"+kindStr, http.StatusSeeOther)
		return
	}
	if errMsg == "" {
		s.touchDashboard() // successful htmx mutations invalidate too
		// The swap replaces only the list, so tell the page to re-fetch the
		// sidebar counts (app.js listens for this event name).
		w.Header().Set("HX-Trigger", "counts-changed")
	}
	items, err := s.catalogs.List(r.Context(), kind)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, kind.Title, kindStr)
	data.Data = s.newCatalogView(r, kindStr, kind, items, errMsg)
	s.renderNamed(w, "catalogs", "catalog_list", data)
}
