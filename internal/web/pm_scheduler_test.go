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
	"testing"
	"time"
)

func TestPMSyncIntervalReloadsSetting(t *testing.T) {
	_, db, srv := newTestServerFull(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	intervals := make(chan time.Duration, 2)
	go srv.RunPMSync(ctx, func(d time.Duration) <-chan time.Time {
		intervals <- d
		return ticks
	})
	select {
	case got := <-intervals:
		if got != 6*time.Hour {
			t.Fatalf("default interval %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not start")
	}
	if _, err := db.Exec("UPDATE settings SET pm_sync_interval_minutes=15 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Now()
	select {
	case got := <-intervals:
		if got != 15*time.Minute {
			t.Fatalf("updated interval %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not reload setting")
	}
}
