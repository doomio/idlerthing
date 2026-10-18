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
	"errors"
	"fmt"
	"strconv"
	"time"

	"idlerthing/internal/model"
)

// Result summarises one provider's sync.
type Result struct {
	Synced  int // services in the feed
	Matched int // linked seedboxes reconciled
	At      time.Time
}

// Effective applies the merge rule for one durable field: the feed updates a
// value only when the user has not made it their own.
//
//	current  is what the inventory holds now
//	previous is what the feed said last time ("" when there was no last time)
//	next     is what the feed says now
//
// An empty current value takes the feed's. A current value equal to the last
// feed value was written by us, so the feed advances it — without this,
// next_due would freeze at the first value it ever saw and every box would
// report as overdue forever. Anything else was typed by a person and wins.
func Effective(current, previous, next string) string {
	if current == "" || current == previous {
		return next
	}
	return current
}

// Sync fetches a provider's feed and reconciles it into the inventory.
//
// The feed is fetched before any transaction is opened: the SQLite pool is a
// single connection, and holding it across a network call would stall every
// other reader. On a fetch failure nothing is written, so the last good cache
// survives and the next sync can recover.
func Sync(ctx context.Context, db *sql.DB, providerID int64, key, url string, now time.Time) (Result, error) {
	feed, err := Fetch(ctx, url, key)
	if err != nil {
		return Result{}, err
	}

	// Snapshot the previous feed values before replacing them: the merge
	// compares against the last value we wrote to tell our writes apart from
	// the user's edits.
	prev, err := (&model.PMStore{DB: db}).List(ctx, providerID)
	if err != nil {
		return Result{}, err
	}
	prevByService := make(map[int64]model.PMService, len(prev))
	for _, s := range prev {
		prevByService[s.ServiceID] = s
	}

	stamp := now.Format(time.RFC3339)
	cached := make([]model.PMService, 0, len(feed.Services))
	for _, s := range feed.Services {
		cached = append(cached, model.PMService{
			ProviderID:       providerID,
			ServiceID:        s.ID,
			Product:          s.Product,
			Group:            s.Group,
			Domain:           s.Domain,
			Status:           s.Status,
			Registered:       s.Registered,
			NextDue:          s.NextDue,
			BillingCycle:     s.BillingCycle,
			RecurringAmount:  float64(s.RecurringAmount),
			Currency:         feed.Currency,
			Username:         s.Username(),
			LoginURL:         accessField(s, func(a *Access) string { return a.LoginURL }),
			PermanentAddress: accessField(s, func(a *Access) string { return a.PermanentAddress }),
			TrackedSince:     usageField(s, func(u *Usage) string { return u.TrackedSince }),
			SentBytes:        usageField(s, func(u *Usage) int64 { return u.SentBytes }),
			ReceivedBytes:    usageField(s, func(u *Usage) int64 { return u.ReceivedBytes }),
			CPUHours:         usageField(s, func(u *Usage) int64 { return u.CPUHours }),
			CPUSince:         usageField(s, func(u *Usage) string { return u.CPUSince }),
			DiskUsedBytes:    usageField(s, func(u *Usage) int64 { return u.DiskUsedBytes }),
			DiskQuotaBytes:   usageField(s, func(u *Usage) int64 { return u.DiskQuotaBytes }),
			Torrents:         usageField(s, func(u *Usage) int64 { return u.Torrents }),
			GeneratedAt:      feed.GeneratedAt,
			SyncedAt:         stamp,
		})
	}
	byService := make(map[int64]model.PMService, len(cached))
	for _, s := range cached {
		byService[s.ServiceID] = s
	}

	res := Result{Synced: len(cached), At: now}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin sync: %w", err)
	}
	defer tx.Rollback()

	if err := (&model.PMStore{DB: db}).ReplaceTx(ctx, tx, providerID, cached); err != nil {
		return Result{}, err
	}

	links, err := model.LinkedSeedboxesTx(ctx, tx, providerID)
	if err != nil {
		return Result{}, err
	}
	for _, link := range links {
		next, ok := byService[link.ServiceID]
		if !ok {
			// The service vanished from the feed (cancelled and pruned, or
			// moved accounts). Leave the inventory alone: it is the user's.
			continue
		}
		before, hadBefore := prevByService[link.ServiceID]
		if err := reconcile(ctx, tx, link, before, hadBefore, next); err != nil {
			return Result{}, err
		}
		res.Matched++
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit sync: %w", err)
	}
	return res, nil
}

// reconcile merges one feed service into one linked seedbox.
func reconcile(ctx context.Context, tx *sql.Tx, link model.PMLink, prev model.PMService, hadPrev bool, next model.PMService) error {
	if !hadPrev {
		// No previous feed value: nothing we wrote, so the feed only fills
		// fields the user left empty.
		prev = model.PMService{}
	}

	// The durable pricing fields, each merged against what the feed said last
	// time. An unmappable cycle leaves price and term untouched: a price
	// stored without its cycle reports a wrong monthly cost.
	term := TermForCycle(next.BillingCycle)
	if term == 0 {
		return nil
	}

	cur, err := currentPricingTx(ctx, tx, link.SeedboxID)
	if err != nil {
		return err
	}

	// Compare amounts as canonically formatted strings so one merge rule
	// covers text and numbers alike.
	priceStr := Effective(formatAmount(cur.price, cur.hasPrice),
		formatAmount(prev.RecurringAmount, true), formatAmount(next.RecurringAmount, true))
	price, err := parseAmountStrict(priceStr)
	if err != nil {
		return fmt.Errorf("merge price %q: %w", priceStr, err)
	}

	currency := Effective(cur.currency, prev.Currency, next.Currency)
	if currency == "" {
		currency = "USD" // pricings.currency is NOT NULL; the feed normally sends one
	}

	if err := model.UpsertPricingTx(ctx, tx, model.ServiceSeedbox, link.SeedboxID, &model.Pricing{
		Currency:    currency,
		Price:       price,
		Term:        term,
		NextDueDate: nullString(Effective(cur.nextDue, prev.NextDue, next.NextDue)),
	}); err != nil {
		return err
	}

	// Disk quota, in the inventory's whole-MB unit. A quota the feed did not
	// report writes nothing rather than zeroing a size the user set.
	quotaMB := next.DiskQuotaBytes / (1024 * 1024)
	if quotaMB <= 0 {
		return nil
	}
	currentDisk, previousDisk := "", ""
	if link.DiskAsMB.Valid {
		currentDisk = strconv.FormatInt(link.DiskAsMB.Int64, 10)
	}
	if hadPrev && prev.DiskQuotaBytes > 0 {
		previousDisk = strconv.FormatInt(prev.DiskQuotaBytes/(1024*1024), 10)
	}
	merged := Effective(currentDisk, previousDisk, strconv.FormatInt(quotaMB, 10))
	if merged == currentDisk {
		return nil
	}
	mb, err := strconv.ParseInt(merged, 10, 64)
	if err != nil {
		return fmt.Errorf("merge disk %q: %w", merged, err)
	}
	return model.SetSeedboxDiskTx(ctx, tx, link.SeedboxID, mb)
}

// currentPricing is the inventory's current pricing values for one seedbox.
type currentPricing struct {
	currency string
	price    float64
	hasPrice bool
	nextDue  string
}

func currentPricingTx(ctx context.Context, tx *sql.Tx, seedboxID int64) (currentPricing, error) {
	var p currentPricing
	var currency, due sql.NullString
	var price sql.NullFloat64
	err := tx.QueryRowContext(ctx, `
		SELECT currency, price, next_due_date FROM pricings
		WHERE service_type = ? AND service_id = ? AND active = 1`,
		model.ServiceSeedbox, seedboxID).Scan(&currency, &price, &due)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("read pricing: %w", err)
	}
	p.currency = currency.String
	p.nextDue = due.String
	p.price = price.Float64
	p.hasPrice = price.Valid
	return p, nil
}

// formatAmount renders an amount for comparison; hasValue false renders "" so
// an absent price merges as "unset" rather than as zero.
func formatAmount(v float64, hasValue bool) string {
	if !hasValue {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// parseAmountStrict is parseAmount without the never-fails behaviour: a
// formatted amount produced above must round-trip.
func parseAmountStrict(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

// accessField reads a field off a service's optional seedbox block.
func accessField(s Service, f func(*Access) string) string {
	if s.Seedbox == nil {
		return ""
	}
	return f(s.Seedbox)
}

// usageField reads a field off a service's optional usage block.
func usageField[T any](s Service, f func(*Usage) T) T {
	var zero T
	if s.Seedbox == nil || s.Seedbox.Usage == nil {
		return zero
	}
	return f(s.Seedbox.Usage)
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
