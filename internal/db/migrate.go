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

package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// deleteTableRe finds delete targets in a migration body. Table names come
// from the trusted embedded SQL files, so COUNT interpolation is safe.
// (Statement-level RowsAffected isn't available from a multi-statement
// Exec, and splitting the body naively breaks on semicolons inside SQL
// comments — 0007 has them.)
var deleteTableRe = regexp.MustCompile(`(?i)DELETE\s+FROM\s+(\w+)`)

// migration is one embedded migration file.
type migration struct {
	version int
	name    string
}

// Migrate applies any pending migrations to db and returns the list of
// migration versions applied during this call. Migrations are tracked via
// PRAGMA user_version; each embedded migrations/NNNN_name.sql file whose
// numeric prefix exceeds the current version is applied in a transaction.
// Delete-bearing migrations log the affected row count per target table.
//
// Before the first pending migration, the database is snapshotted to
// <db>.pre-<version>.sqlite. Several migrations rewrite data, not just schema
// (0007 deletes orphans, 0009/0010 convert units, 0011/0012 rebuild the notes
// CHECK constraint), so a crash or a wrong assumption mid-upgrade needs a
// rollback target that does not depend on the operator having copied the file
// beforehand. Producing that snapshot is a precondition: if it fails, the
// upgrade aborts instead of running without it.
func Migrate(db *sql.DB) ([]int, error) {
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return nil, fmt.Errorf("read user_version: %w", err)
	}
	pending, err := pendingMigrations(current)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	// user_version 0 means the file has no schema yet — a first run, with
	// nothing worth snapshotting.
	if current > 0 {
		if err := snapshotBeforeUpgrade(db, pending[0].version); err != nil {
			return nil, err
		}
	}
	var applied []int
	for _, m := range pending {
		if err := applyMigration(db, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}

// pendingMigrations lists the embedded migrations beyond current, sorted by
// version. A DB from a newer build is refused.
func pendingMigrations(current int) ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var pending []migration
	maxVersion := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad numeric prefix: %w", e.Name(), err)
		}
		if v > maxVersion {
			maxVersion = v
		}
		if v > current {
			pending = append(pending, migration{version: v, name: e.Name()})
		}
	}
	if current > maxVersion {
		return nil, fmt.Errorf("database was created by a newer version of idlerthing (user_version %d > %d)", current, maxVersion)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	return pending, nil
}

// snapshotBeforeUpgrade writes a consistent copy of the database to
// <db>.pre-<version>.sqlite, where version is the first migration about to be
// applied. WAL mode means the newest commits can still be sitting in
// <db>-wal, so the WAL is checkpointed (TRUNCATE) before the file is copied.
// A checkpoint that reports busy means a second writer is live — a state this
// single-process app never expects, and copying under it would yield a snapshot
// that will not restore cleanly — so it is reported as an error rather than
// skipped.
func snapshotBeforeUpgrade(db *sql.DB, version int) error {
	path := mainDBPath(db)
	if path == "" || path == ":memory:" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat database for backup: %w", err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("read journal mode: %w", err)
	}
	if strings.EqualFold(mode, "wal") {
		var busy, logPages, checkpointed int
		if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointed); err != nil {
			return fmt.Errorf("checkpoint before backup: %w", err)
		}
		if busy != 0 {
			return fmt.Errorf("cannot snapshot the database: another writer holds the WAL lock (%s)", path)
		}
	}
	dst := fmt.Sprintf("%s.pre-%04d.sqlite", path, version)
	if err := copyFile(path, dst); err != nil {
		return fmt.Errorf("backup before migration %04d: %w", version, err)
	}
	slog.Info("database snapshot written", "path", dst)
	return nil
}

// mainDBPath returns the file behind the main database, or "" when there is
// none to copy (in-memory databases, or a driver that does not answer pragma).
func mainDBPath(db *sql.DB) string {
	var seq int
	var name, file string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		return ""
	}
	if name != "main" {
		return ""
	}
	return file
}

// copyFile copies src onto dst atomically: the bytes land in a temp file that
// is fsynced and then renamed, so an interrupted copy can never masquerade as a
// usable backup. 0600 is forced after the copy rather than left to the umask —
// the snapshot contains sessions, tokens and the admin hash.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail(err)
	}
	if err := out.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// applyMigration runs one migration in a transaction, logging the rows each
// DELETE removes (counted before/after — see deleteTableRe).
func applyMigration(db *sql.DB, m migration) error {
	body, err := migrationsFS.ReadFile("migrations/" + m.name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", m.name, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer tx.Rollback()

	before := map[string]int{}
	for _, dm := range deleteTableRe.FindAllStringSubmatch(string(body), -1) {
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM " + dm[1]).Scan(&n); err == nil {
			before[dm[1]] = n
		}
	}
	if _, err := tx.Exec(string(body)); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	for table, n := range before {
		var after int
		if err := tx.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&after); err == nil && after != n {
			slog.Info("migration deleted rows", "migration", m.name, "table", table, "rows", n-after)
		}
	}
	// user_version cannot be set as a bound parameter; version comes from
	// the trusted embedded filename so interpolation is safe.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("set user_version for %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}
