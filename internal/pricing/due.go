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

package pricing

import (
	"context"
	"database/sql"
	"time"

	"idlerthing/internal/model"
)

// AdvanceDueDates lazily rolls past-due recurring pricings forward: any
// active pricing whose next_due_date is before today (and whose term is not
// one-time) is advanced by its term until it lands on or after today. Month
// arithmetic aims at the row's stored day-of-month anchor (due_day) and is
// clamped to short months, so a February rollover of a Jan-31 billing does
// not re-anchor March to the 28th. When the advanced row is a seedbox linked
// to a provider feed, the new date is written back into the feed cache as
// our baseline — otherwise the next sync would read the rollover as a human
// edit and freeze the provider's due-date updates forever. Returns the
// number of rows updated.
func AdvanceDueDates(ctx context.Context, db *sql.DB) (int, error) {
	today := time.Now().Format(time.DateOnly)

	// Select candidates and update INSIDE one transaction, with a
	// compare-and-swap WHERE on the old date — a concurrent pricing edit
	// between select and update is never overwritten.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, term, next_due_date, due_day, service_type, service_id
		FROM pricings
		WHERE active = 1 AND next_due_date IS NOT NULL AND next_due_date < ?
		  AND term != ?`, today, model.TermOneTime)
	if err != nil {
		return 0, err
	}
	type dueRow struct {
		id          int64
		term        int
		due         string
		dueDay      sql.NullInt64
		serviceType int
		serviceID   int64
	}
	var pending []dueRow
	for rows.Next() {
		var dr dueRow
		if err := rows.Scan(&dr.id, &dr.term, &dr.due, &dr.dueDay, &dr.serviceType, &dr.serviceID); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, dr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	updated := 0
	// Compare against the SAME local "today" the SELECT used, not UTC
	// midnight — otherwise early-morning rows in UTC+N zones get selected
	// but never advanced.
	todayStart, _ := time.Parse(time.DateOnly, today)
	for _, dr := range pending {
		due, err := time.Parse(time.DateOnly, dr.due)
		if err != nil {
			continue // unparseable stored value — leave it alone
		}
		months := model.TermMonths(dr.term)
		if months == 0 {
			continue
		}
		anchor := 0
		if dr.dueDay.Valid {
			anchor = int(dr.dueDay.Int64)
		}
		due = catchUp(due, months, todayStart, anchor)
		next := due.Format(time.DateOnly)
		// The rollover deliberately leaves due_day untouched: it is the
		// anchor, not an output.
		res, err := tx.ExecContext(ctx,
			"UPDATE pricings SET next_due_date = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND next_due_date = ?",
			next, dr.id, dr.due)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // changed concurrently — leave it alone
		}
		if dr.serviceType == model.ServiceSeedbox {
			if err := model.BumpFeedDueBaselineTx(ctx, tx, dr.serviceID, next); err != nil {
				return updated, err
			}
		}
		updated++
	}
	return updated, tx.Commit()
}

// catchUp advances due by whole terms until it is on or after today. Each
// step lands in the k-th target month aiming at anchorDay (the date's own
// day when no anchor is known), computed from the ORIGINAL month so one
// short month cannot re-anchor the whole series: with anchor 31, Jan 31 →
// Feb 28 → Mar 31, never Mar 28.
func catchUp(due time.Time, months int, today time.Time, anchorDay int) time.Time {
	next := due
	for k := 1; next.Before(today); k++ {
		next = addMonthsAnchored(due, k*months, anchorDay)
	}
	return next
}

// AddMonthsClamped adds months to t, clamping the day of month when the
// target month is shorter (Jan 31 + 1 month → Feb 28/29).
func AddMonthsClamped(t time.Time, months int) time.Time {
	return addMonthsAnchored(t, months, 0)
}

// addMonthsAnchored moves t months forward aiming for anchorDay (t's own day
// when anchorDay is outside 1..31), clamped to the last day of a shorter
// target month.
func addMonthsAnchored(t time.Time, months, anchorDay int) time.Time {
	y, m, d := t.Date()
	if anchorDay >= 1 && anchorDay <= 31 {
		d = anchorDay
	}
	// First of the target month, then find its length.
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	lastDay := first.AddDate(0, 1, -1).Day()
	if d > lastDay {
		d = lastDay
	}
	return time.Date(first.Year(), first.Month(), d,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}
