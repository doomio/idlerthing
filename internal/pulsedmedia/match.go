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
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"idlerthing/internal/model"
)

// Match links a provider's seedboxes to its cached feed services and returns
// how many links it made.
//
// The cost of a wrong link is a wrong renewal date and price on the user's
// row, so matching only takes a link it can justify: an exact host, an exact
// username, or a provider that has exactly one service. Anything else stays
// unlinked for the user to connect by hand.
func Match(ctx context.Context, db *sql.DB, providerID int64) (int, error) {
	services, err := (&model.PMStore{DB: db}).List(ctx, providerID)
	if err != nil {
		return 0, err
	}
	if len(services) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin match: %w", err)
	}
	defer tx.Rollback()

	candidates, err := model.UnlinkedSeedboxesTx(ctx, tx, providerID)
	if err != nil {
		return 0, err
	}

	linked := 0
	for _, c := range candidates {
		serviceID, ok := matchService(c.Hostname, services)
		if !ok {
			continue
		}
		if err := model.LinkSeedboxTx(ctx, tx, c.SeedboxID, serviceID); err != nil {
			return 0, err
		}
		linked++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit match: %w", err)
	}
	return linked, nil
}

// matchService decides which feed service a seedbox hostname belongs to.
func matchService(hostname string, services []model.PMService) (int64, bool) {
	// An exact host is the strongest signal there is.
	for _, s := range services {
		if hostMatch(hostname, s.PermanentAddress) || strings.EqualFold(s.Domain, hostname) {
			return s.ServiceID, true
		}
	}

	// Otherwise the account username usually appears in the hostname
	// ("user.box.host"). Two hits is ambiguity, not agreement.
	var found int64
	hits := 0
	lowerHost := strings.ToLower(hostname)
	for _, s := range services {
		if s.Username == "" {
			continue
		}
		if strings.Contains(lowerHost, strings.ToLower(s.Username)) {
			found = s.ServiceID
			hits++
		}
	}
	if hits == 1 {
		return found, true
	}
	if hits > 1 {
		return 0, false
	}

	// One service on the account can only be that service.
	if len(services) == 1 {
		return services[0].ServiceID, true
	}
	return 0, false
}

// hostMatch reports whether a permanent address names the given host. The
// field is sometimes a bare host and sometimes a URL.
func hostMatch(hostname, permanentAddress string) bool {
	if permanentAddress == "" {
		return false
	}
	if u, err := url.Parse(permanentAddress); err == nil && u.Hostname() != "" {
		return strings.EqualFold(u.Hostname(), hostname)
	}
	return strings.EqualFold(permanentAddress, hostname)
}
