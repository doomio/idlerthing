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
	"log/slog"
	"time"
)

// RunPMSync refreshes keyed providers at startup and then periodically.
// The interval is read after each wake, so Settings changes take effect on the next cycle.
func (s *Server) RunPMSync(ctx context.Context, after func(time.Duration) <-chan time.Time) {
	if ctx.Err() == nil {
		s.syncPM(ctx)
	}
	for ctx.Err() == nil {
		var minutes int
		if err := s.db.QueryRowContext(ctx, "SELECT pm_sync_interval_minutes FROM settings WHERE id=1").Scan(&minutes); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("read PM sync interval", "err", err)
			minutes = 15
		}
		if minutes < 15 || minutes > 10080 {
			minutes = 360
		}
		select {
		case <-ctx.Done():
			return
		case <-after(time.Duration(minutes) * time.Minute):
		}
		if ctx.Err() == nil {
			s.syncPM(ctx)
		}
	}
}
