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

package model

import (
	"context"
	"database/sql"
	"fmt"
)

// PMService is one service in a provider's cached customer-API feed. Every
// field is derived data: the rows are replaced wholesale by each sync, so
// nothing here is authoritative and nothing references it by foreign key.
//
// The usage fields (SentBytes, ReceivedBytes, CPUHours, DiskUsedBytes,
// Torrents) are volatile. They are rendered from the cache and must never be
// written into the inventory columns.
type PMService struct {
	ProviderID       int64
	ServiceID        int64
	Product          string
	Group            string
	Domain           string
	Status           string
	Registered       string
	NextDue          string
	BillingCycle     string
	RecurringAmount  float64
	Currency         string
	Username         string
	LoginURL         string
	PermanentAddress string

	TrackedSince   string
	SentBytes      int64
	ReceivedBytes  int64
	CPUHours       int64
	CPUSince       string
	DiskUsedBytes  int64
	DiskQuotaBytes int64
	Torrents       int64

	GeneratedAt string
	SyncedAt    string
}

// pmColumns is the canonical column order; scanPMService and the INSERT in
// Replace both follow it.
const pmColumns = `provider_id, service_id, product, grp, domain, status,
	registered, next_due, billing_cycle, recurring_amount, currency, username,
	login_url, permanent_address, tracked_since, sent_bytes, received_bytes,
	cpu_hours, cpu_since, disk_used_bytes, disk_quota_bytes, torrents,
	generated_at, synced_at`

// PMStore wraps the DB for the pm_services cache.
type PMStore struct {
	DB *sql.DB
}

// Replace swaps every cached service of one provider for the given set, in a
// single transaction. An empty or nil slice clears the provider's cache — that
// is what a feed response with no services should do.
func (st *PMStore) Replace(ctx context.Context, providerID int64, services []PMService) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pm_services replace: %w", err)
	}
	defer tx.Rollback()
	if err := st.ReplaceTx(ctx, tx, providerID, services); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceTx swaps a provider's cached services inside a transaction the caller
// owns. A sync uses this so the cache and the inventory it reconciles commit
// together: a half-applied sync would look like a human edit to the next one
// and strand the value it failed to write.
func (st *PMStore) ReplaceTx(ctx context.Context, tx *sql.Tx, providerID int64, services []PMService) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM pm_services WHERE provider_id = ?", providerID); err != nil {
		return fmt.Errorf("clear pm_services: %w", err)
	}
	if len(services) > 0 {
		ins, err := tx.PrepareContext(ctx, `INSERT INTO pm_services (`+pmColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare pm_services insert: %w", err)
		}
		defer ins.Close()
		for i := range services {
			s := &services[i]
			if _, err := ins.ExecContext(ctx,
				providerID, s.ServiceID, s.Product, s.Group, s.Domain, s.Status,
				s.Registered, s.NextDue, s.BillingCycle, s.RecurringAmount,
				s.Currency, s.Username, s.LoginURL, s.PermanentAddress,
				s.TrackedSince, s.SentBytes, s.ReceivedBytes, s.CPUHours,
				s.CPUSince, s.DiskUsedBytes, s.DiskQuotaBytes, s.Torrents,
				s.GeneratedAt, s.SyncedAt); err != nil {
				return fmt.Errorf("insert pm_services %d: %w", s.ServiceID, err)
			}
		}
	}
	return nil
}

// List returns one provider's cached services, ordered by service id.
func (st *PMStore) List(ctx context.Context, providerID int64) ([]PMService, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx,
		"SELECT "+pmColumns+" FROM pm_services WHERE provider_id = ? ORDER BY service_id",
		providerID)
	if err != nil {
		return nil, fmt.Errorf("list pm_services: %w", err)
	}
	defer rows.Close()

	var out []PMService
	for rows.Next() {
		var s PMService
		if err := rows.Scan(&s.ProviderID, &s.ServiceID, &s.Product, &s.Group,
			&s.Domain, &s.Status, &s.Registered, &s.NextDue, &s.BillingCycle,
			&s.RecurringAmount, &s.Currency, &s.Username, &s.LoginURL,
			&s.PermanentAddress, &s.TrackedSince, &s.SentBytes, &s.ReceivedBytes,
			&s.CPUHours, &s.CPUSince, &s.DiskUsedBytes, &s.DiskQuotaBytes,
			&s.Torrents, &s.GeneratedAt, &s.SyncedAt); err != nil {
			return nil, fmt.Errorf("scan pm_services: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PMLink is a seedbox linked to one of a provider's feed services, with the
// inventory values a sync has to merge against.
type PMLink struct {
	SeedboxID int64
	ServiceID int64
	DiskAsMB  sql.NullInt64 // NULL when the user has set no size
}

// LinkedSeedboxesTx lists the seedboxes of a provider that are linked to a
// feed service. Unlinked seedboxes are not the sync's business.
func LinkedSeedboxesTx(ctx context.Context, tx *sql.Tx, providerID int64) ([]PMLink, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, pm_service_id, disk_as_mb FROM seedboxes
		WHERE provider_id = ? AND pm_service_id IS NOT NULL AND pm_manual_override = 0
		ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list linked seedboxes: %w", err)
	}
	defer rows.Close()

	var out []PMLink
	for rows.Next() {
		var l PMLink
		if err := rows.Scan(&l.SeedboxID, &l.ServiceID, &l.DiskAsMB); err != nil {
			return nil, fmt.Errorf("scan linked seedbox: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PMCandidate is a provider's seedbox that has no feed link yet.
type PMCandidate struct {
	SeedboxID int64
	Hostname  string
}

// UnlinkedSeedboxesTx lists a provider's seedboxes with no feed link, the only
// rows a match may touch.
func UnlinkedSeedboxesTx(ctx context.Context, tx *sql.Tx, providerID int64) ([]PMCandidate, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, hostname FROM seedboxes
		WHERE provider_id = ? AND pm_service_id IS NULL
		ORDER BY id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list unlinked seedboxes: %w", err)
	}
	defer rows.Close()

	var out []PMCandidate
	for rows.Next() {
		var c PMCandidate
		if err := rows.Scan(&c.SeedboxID, &c.Hostname); err != nil {
			return nil, fmt.Errorf("scan unlinked seedbox: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LinkSeedboxTx stores a feed link on a seedbox.
func LinkSeedboxTx(ctx context.Context, tx *sql.Tx, seedboxID, serviceID int64) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE seedboxes SET pm_service_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		serviceID, seedboxID)
	if err != nil {
		return fmt.Errorf("link seedbox: %w", err)
	}
	return nil
}

// BumpFeedDueBaselineTx records an auto-advanced due date as the last value
// WE wrote into a linked seedbox's feed cache row. pulsedmedia.Sync tells its
// own writes apart from human edits by comparing the inventory against that
// cached value; without this write-back the dashboard's lazy rollover would
// look like a human edit on the next sync and freeze the provider's
// due-date updates forever. Only advances a baseline matching oldDue, so a
// human-edited due date stays distinct. A no-op without a complete feed link.
func BumpFeedDueBaselineTx(ctx context.Context, tx *sql.Tx, seedboxID int64, oldDue, nextDue string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE pm_services SET next_due = ?
		WHERE next_due = ? AND (provider_id, service_id) IN (
			SELECT provider_id, pm_service_id FROM seedboxes
			WHERE id = ? AND pm_service_id IS NOT NULL AND provider_id IS NOT NULL)`,
		nextDue, oldDue, seedboxID)
	if err != nil {
		return fmt.Errorf("bump feed due baseline: %w", err)
	}
	return nil
}

// SetSeedboxDiskTx writes only the disk size of a seedbox. It is a narrow
// write on purpose: a sync must not touch the rest of a user's seedbox row.
func SetSeedboxDiskTx(ctx context.Context, tx *sql.Tx, seedboxID, diskMB int64) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE seedboxes SET disk_as_mb = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		diskMB, seedboxID)
	if err != nil {
		return fmt.Errorf("set seedbox disk: %w", err)
	}
	return nil
}
