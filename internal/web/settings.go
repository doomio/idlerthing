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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/importer"
	"idlerthing/internal/prom"
)

// settingsView is the template payload for GET /settings.
type settingsView struct {
	PMSyncIntervalMinutes int
	DefaultCurrency       string
	DashboardCurrency     string
	DueSoon               int
	RecentlyAdded         int
	Theme                 string
	ServersPublic         bool
	AccentColor           string
	Compact               bool
	PrometheusEnabled     bool
	PrometheusURL         string
	Currencies            []string
	TokenSet              bool
	RevealedToken         string // shown once after generation
	Version               string // build version, shown in the About panel
	Errors                map[string]string
}

// handleSettingsGet renders GET /settings.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, settingsView{})
}

// renderSettings renders the settings page, overlaying the current state
// via the per-request memo (also correct on the POST-error path: the memo
// is per-request and the success path redirects).
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, view settingsView) {
	row := s.memoSettings(r)
	view.PMSyncIntervalMinutes = row.PMSyncIntervalMinutes
	view.DefaultCurrency = row.DefaultCurrency
	view.DashboardCurrency = row.DashboardCurrency
	view.DueSoon = row.DueSoon
	view.RecentlyAdded = row.RecentlyAdded
	view.Theme = row.Theme
	view.ServersPublic = row.ServersPublic
	view.AccentColor = row.AccentColor
	view.Compact = row.Compact
	view.PrometheusEnabled = row.PrometheusEnabled
	view.PrometheusURL = row.PrometheusURL
	view.Currencies = currencies
	view.Version = s.Version()
	view.RevealedToken = s.popRevealedToken(w, r)

	u := userFromCtx(r.Context())
	if u != nil {
		var tokenHash *string
		s.db.QueryRowContext(r.Context(),
			"SELECT api_token_hash FROM users WHERE id = ?", u.ID).Scan(&tokenHash)
		view.TokenSet = tokenHash != nil && *tokenHash != ""
	}

	data := s.newPageData(w, r, "Settings", "settings")
	data.Data = view
	s.render(w, r, "settings", data)
}

// handleSettingsImport imports an uploaded export without allowing native JSON
// to duplicate existing inventory. The importer owns its transaction.
func (s *Server) handleSettingsImport(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "select an import file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	format, input, err := importer.DetectFormat(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var message string
	switch format {
	case importer.FormatNative:
		summary, importErr := importer.Import(r.Context(), s.db, input, false)
		err = importErr
		if err == nil {
			message = fmt.Sprintf("Imported %d servers and %d seedboxes (%d warnings).", summary.Servers, summary.Seedboxes, len(summary.Warnings))
		}
	case importer.FormatMyJSON, importer.FormatMyCSV:
		var records []importer.MyServer
		var warnings []string
		if format == importer.FormatMyJSON {
			records, warnings, err = importer.ParseMyJSON(input)
		} else {
			records, warnings, err = importer.ParseMyCSV(input)
		}
		if err == nil {
			var summary *importer.MySummary
			summary, err = importer.ImportMyIdlers(r.Context(), s.db, records, warnings)
			if err == nil {
				message = fmt.Sprintf("Imported %d servers; skipped %d duplicates (%d warnings).", summary.Imported, summary.SkippedDup, len(summary.Warnings))
			}
		}
	default:
		err = fmt.Errorf("unsupported import format")
	}
	if err != nil {
		http.Error(w, "import failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.setFlash(w, r, "ok", message)
	http.Redirect(w, r, routeSettings, http.StatusSeeOther)
}

// handleSettingsUpdate handles POST /settings (general + display sections).
func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	errs := map[string]string{}

	defaultCur := validCurrency(r.FormValue("default_currency"))
	dashCur := validCurrency(r.FormValue("dashboard_currency"))
	theme := r.FormValue("theme")
	if theme != "dark" && theme != "light" {
		theme = "dark"
	}
	dueSoon, err := strconv.Atoi(r.FormValue("due_soon_amount"))
	if err != nil || dueSoon < 1 || dueSoon > 365 {
		errs["due_soon_amount"] = "Must be between 1 and 365."
	}
	recent, err := strconv.Atoi(r.FormValue("recently_added_amount"))
	if err != nil || recent < 1 || recent > 100 {
		errs["recently_added_amount"] = "Must be between 1 and 100."
	}
	interval, err := strconv.Atoi(r.FormValue("pm_sync_interval_minutes"))
	if r.FormValue("pm_sync_interval_minutes") == "" {
		interval = s.memoSettings(r).PMSyncIntervalMinutes
	}
	if interval < 15 || interval > 10080 {
		errs["pm_sync_interval_minutes"] = "Must be between 15 and 10080 minutes."
	}
	serversPublic := r.FormValue("servers_public") != ""
	compact := r.FormValue("compact_mode") != ""
	promEnabled := r.FormValue("prometheus_enabled") != ""
	promURL := strings.TrimSpace(r.FormValue("prometheus_url"))
	if promEnabled && !validPromURL(promURL) {
		errs["prometheus_url"] = "Must be an http(s) URL, e.g. http://localhost:9090."
	}

	accent := strings.ToLower(strings.TrimSpace(r.FormValue("accent_color")))
	if accent == "" {
		// Field absent (older clients) — keep the current (memoized) value.
		accent = s.memoSettings(r).AccentColor
		if !accentColorRe.MatchString(accent) {
			accent = defaultAccent
		}
	} else if !accentColorRe.MatchString(accent) {
		errs["accent_color"] = "Must be a hex color like #5b9cf8."
	}

	if len(errs) > 0 {
		s.setFlash(w, r, "err", "Please fix the errors below.")
		s.renderSettings(w, r, settingsView{Errors: errs})
		return
	}

	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE settings SET default_currency = ?, dashboard_currency = ?,
			due_soon_amount = ?, recently_added_amount = ?, theme = ?,
			servers_public = ?, accent_color = ?, compact_mode = ?,
			prometheus_enabled = ?, prometheus_url = ?, pm_sync_interval_minutes = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = 1`,
		defaultCur, dashCur, dueSoon, recent, theme, boolToIntWeb(serversPublic),
		accent, boolToIntWeb(compact), boolToIntWeb(promEnabled), promURL, interval); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", "Settings saved.")
	http.Redirect(w, r, routeSettings, http.StatusSeeOther)
}

// handleSettingsAccount handles POST /settings/account (action=password|token).
func (s *Server) handleSettingsAccount(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	switch r.FormValue("action") {
	case "password":
		s.changePassword(w, r, u)
	case "token":
		s.generateAPIToken(w, r, u)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// changePassword validates + updates the password, killing other sessions.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u *user) {
	current := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	// Same per-source budget as /login: a stolen session cookie must not
	// buy unthrottled online guessing of the real password (which is what
	// survives a session wipe and may be reused elsewhere). Checked before
	// the bcrypt verify, so refused attempts are cheap.
	ip := s.clientIP(r)
	if !s.limit.allow(limiterKey(ip)) {
		s.logBlocked(ip)
		s.setFlash(w, r, "err", "Too many attempts. Try again later.")
		http.Redirect(w, r, routeSettings, http.StatusSeeOther)
		return
	}

	var hash string
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT password_hash FROM users WHERE id = ?", u.ID).Scan(&hash); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		// Same proxy-aware source as the login failures, so one fail2ban
		// filter catches an attacker who got as far as a session too.
		slog.Warn("login: failed password-change verification", "from", ip)
		s.setFlash(w, r, "err", "Current password is wrong.")
		http.Redirect(w, r, routeSettings, http.StatusSeeOther)
		return
	}
	if len(newPass) < 8 || len(newPass) > 72 {
		// bcrypt reads at most 72 bytes — reject longer instead of 500ing.
		s.setFlash(w, r, "err", "New password must be 8–72 characters.")
		http.Redirect(w, r, routeSettings, http.StatusSeeOther)
		return
	}
	if newPass != confirm {
		s.setFlash(w, r, "err", "New passwords do not match.")
		http.Redirect(w, r, routeSettings, http.StatusSeeOther)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE users SET password_hash = ?, api_token_hash = NULL,
			updated_at = CURRENT_TIMESTAMP WHERE id = ? AND password_hash = ?`,
		string(newHash), u.ID, hash)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	rows, err := res.RowsAffected()
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if rows != 1 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// Invalidate ALL sessions for the user — including the current one,
	// since a stolen copy of that cookie would otherwise stay valid. A
	// fresh session is issued on the response right after.
	if _, err := tx.ExecContext(r.Context(),
		"DELETE FROM sessions WHERE user_id = ?", u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if err := s.createSession(w, r, u.ID, string(newHash)); err != nil {
		if errors.Is(err, errCredentialsChanged) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.setFlash(w, r, "ok", "Password changed. All sessions were rotated and the API token was revoked.")
	http.Redirect(w, r, routeSettings, http.StatusSeeOther)
}

// apiTokenCookieName carries the freshly generated API token across the
// PRG redirect — the plaintext is shown exactly once.
const apiTokenCookieName = "idler_api_token"

// generateAPIToken creates a new API token, stores its sha256, and redirects
// (PRG) with the plaintext riding a one-time cookie — rendering it directly
// with a 200 would regenerate+revoke the token on every F5.
func (s *Server) generateAPIToken(w http.ResponseWriter, r *http.Request, u *user) {
	token, err := randomToken(32)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := s.db.ExecContext(r.Context(),
		"UPDATE users SET api_token_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		hex.EncodeToString(sum[:]), u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     apiTokenCookieName,
		Value:    token,
		Path:     routeSettings,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, routeSettings, http.StatusSeeOther)
}

// popRevealedToken reads and clears the one-time API-token cookie.
func (s *Server) popRevealedToken(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie(apiTokenCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	// Same attributes as the cookie being cleared: a non-Secure clear can be
	// refused where the original was Secure, leaving the token visible.
	http.SetCookie(w, &http.Cookie{
		Name: apiTokenCookieName, Value: "", Path: routeSettings, HttpOnly: true,
		Secure: s.cookieSecure(r), MaxAge: -1,
	})
	return cookie.Value
}

// validCurrency returns the code when supported, else USD.
func validCurrency(code string) string {
	for _, c := range currencies {
		if code == c {
			return code
		}
	}
	return "USD"
}

// boolToIntWeb converts a bool to the 0/1 ints the settings columns store.
func boolToIntWeb(b bool) int {
	if b {
		return 1
	}
	return 0
}

// handlePrometheusTest handles POST /settings/prometheus/test — runs an `up`
// instant query against the URL in the form and flashes the outcome.
func (s *Server) handlePrometheusTest(w http.ResponseWriter, r *http.Request) {
	// The typed value, so an operator can validate an endpoint BEFORE saving
	// it — reading the saved one instead would test a different server than
	// the field beside the button names. Falls back to the saved URL when the
	// form carries no field at all (the CLI/retry path). Nothing is written.
	baseURL := strings.TrimSpace(r.FormValue("prometheus_url"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(s.memoSettings(r).PrometheusURL)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" || !validPromURL(baseURL) {
		s.setFlash(w, r, "err", "No valid Prometheus URL to test — enter one (http:// or https:// with a host).")
		http.Redirect(w, r, routeSettings, http.StatusSeeOther)
		return
	}
	samples, err := prom.New(baseURL).Query(r.Context(), "up")
	if err != nil {
		s.setFlash(w, r, "err", "Connection failed: "+err.Error())
		http.Redirect(w, r, routeSettings, http.StatusSeeOther)
		return
	}
	up := 0
	for _, sample := range samples {
		if sample.Value == 1 {
			up++
		}
	}
	s.setFlash(w, r, "ok", "Connected — "+strconv.Itoa(up)+" of "+strconv.Itoa(len(samples))+" targets up.")
	http.Redirect(w, r, routeSettings, http.StatusSeeOther)
}
