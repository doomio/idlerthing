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
