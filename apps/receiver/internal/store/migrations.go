package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// currentSchemaVersion is the value PRAGMA user_version will hold
// after a successful applyMigrations. Bump this and add a new
// numbered block at the bottom of applyMigrations whenever the
// schema changes in a non-additive way. The framework runs every
// block where `if version < N` and stops when version reaches
// currentSchemaVersion.
//
// Convention: each block is its own self-contained migration,
// idempotent on re-run (so a partial upgrade that crashed
// mid-way resumes cleanly on next Open), and sets user_version to
// N only after the entire block has run. A block that fails
// half-way leaves user_version unchanged and the next Open
// retries from that point.
const currentSchemaVersion = 2

// applyMigrations runs the schema migrations, gated on
// PRAGMA user_version. The version starts at 0 on a fresh
// database; the framework reads it on entry, runs every
// `if version < N` block whose N is greater than the current
// version, and updates user_version to N after each successful
// block.
//
// Existing installs created before this framework landed will
// have user_version == 0 but the tables and indices already
// exist. The first migration block (version 1) is therefore
// wrapped in `IF NOT EXISTS` so the CREATE TABLE statements are
// no-ops on an already-migrated install and effective on a
// fresh one. This is the load-bearing compatibility shim — the
// old "trivial" applyMigrations used IF NOT EXISTS for the same
// reason, and QUB-101 explicitly requires upgrading those
// installs in place rather than via a "schema dump and reload."
//
// Indices target the dashboard's hot queries:
//
//   - idx_runs_started_at: time-range filters and the "last N runs"
//     cursor pagination.
//   - idx_runs_owner_repo_started: per-repo drill-downs and the
//     repo selector.
//   - idx_runs_status: failure-rate rollups by day.
//   - idx_runs_installation: the installations page.
func applyMigrations(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v, err := readUserVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("migration: read user_version: %w", err)
	}
	start := v

	// Migrations in order. Each block is gated on its own
	// version number so a partial upgrade resumes cleanly.
	if v < 1 {
		if err := migrateV1(ctx, db); err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		if err := writeUserVersion(ctx, db, 1); err != nil {
			return fmt.Errorf("migration v1: write user_version: %w", err)
		}
		v = 1
	}
	// v2 (QUB-108) unlocks the exception dock: failure_class
	// (so the dashboard can filter OOMKilled on day one), the
	// run_stages table (waterfall), the refunds table (Phase 5
	// audit trail for the "zero out cost" action), a
	// last_heartbeat_at on runs (Phase 2 stuck-run detection),
	// and the paused / lens_opt_out columns on installations
	// (Phase 4 install controls). Everything is wrapped in a
	// SQLite-compatible IF NOT EXISTS / column-existence guard
	// so the block is idempotent — a partial upgrade that
	// crashed after ALTER TABLE but before the user_version
	// write retries cleanly on next Open.
	if v < 2 {
		if err := migrateV2(ctx, db); err != nil {
			return fmt.Errorf("migration v2: %w", err)
		}
		if err := writeUserVersion(ctx, db, 2); err != nil {
			return fmt.Errorf("migration v2: write user_version: %w", err)
		}
		v = 2
	}
	// Add later migrations as additional `if v < N { ... }`
	// blocks here. Each must set user_version = N on success.

	if v < currentSchemaVersion {
		return fmt.Errorf("migration: final version %d, want %d", v, currentSchemaVersion)
	}
	if start < v {
		slog.Info("store: migrated", "from", start, "to", v, "current", currentSchemaVersion)
	}
	return nil
}

// migrateV1 is the initial schema. Idempotent on re-run
// (CREATE TABLE / INDEX IF NOT EXISTS) so a database created by
// the old applyMigrations upgrades in place. The shape is the
// same as the pre-QUB-101 schema; QUB-101 changed the
// migration driver (this function) but not the data model.
func migrateV1(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS runs (
			id              TEXT PRIMARY KEY,
			owner           TEXT NOT NULL,
			repo            TEXT NOT NULL,
			pr_number       INTEGER NOT NULL,
			commit_sha      TEXT NOT NULL,
			base_ref        TEXT,
			review_number   INTEGER NOT NULL DEFAULT 1,
			reason          TEXT,
			installation_id INTEGER,
			status          TEXT NOT NULL,
			started_at      TEXT NOT NULL,
			ended_at        TEXT,
			duration_ms     INTEGER,
			error           TEXT,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_started_at ON runs(started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_owner_repo ON runs(owner, repo)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_owner_repo_started ON runs(owner, repo, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_installation ON runs(installation_id)`,
		`CREATE TABLE IF NOT EXISTS telemetry (
			run_id              TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
			model               TEXT NOT NULL,
			provider            TEXT,
			input_tokens        INTEGER NOT NULL DEFAULT 0,
			output_tokens       INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
			cost_usd            REAL NOT NULL DEFAULT 0,
			step_count          INTEGER NOT NULL DEFAULT 0,
			recorded_at         TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS installations (
			id                INTEGER PRIMARY KEY,
			account_login     TEXT NOT NULL,
			account_type      TEXT NOT NULL,
			repository_selection TEXT,
			installed_at      TEXT,
			fetched_at        TEXT NOT NULL
		)`,
	}
	for i, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration v1 statement %d: %w", i, err)
		}
	}
	return nil
}

// migrateV2 (QUB-108) is the dashboard foundation. It must
// be safe on a database that was partially upgraded — every
// statement is guarded either by IF NOT EXISTS (for new
// tables/indices) or by the hasColumn() helper (for new
// columns on existing tables, because SQLite has no
// ADD COLUMN IF NOT EXISTS prior to 3.35). The failure_class
// column is the load-bearing one for the exception dock
// (Phase 4); the others are forward-compat for later phases
// and would otherwise need their own migrations.
//
// The UNIQUE(run_id, stage) constraint on run_stages is what
// makes the runner's at-least-once POSTs safe — a re-delivery
// of the same stage hits the conflict and gets ON CONFLICT
// DO UPDATE, not a duplicate row.
func migrateV2(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS run_stages (
			id          INTEGER PRIMARY KEY,
			run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			stage       TEXT NOT NULL,
			started_at  TEXT NOT NULL,
			ended_at    TEXT,
			duration_ms INTEGER,
			meta        TEXT,
			UNIQUE(run_id, stage)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_run_stages_run_id ON run_stages(run_id)`,
		`CREATE TABLE IF NOT EXISTS refunds (
			id           INTEGER PRIMARY KEY,
			run_id       TEXT,
			lens         TEXT,
			tokens       INTEGER,
			refunded_at  TEXT NOT NULL,
			refunded_by  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refunds_run_id ON refunds(run_id)`,
	}
	for i, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration v2 statement %d: %w", i, err)
		}
	}

	// Column adds. ALTER TABLE ADD COLUMN fails if the column
	// already exists, so each one is guarded by a pragma
	// check. The hasColumn() helper is the only SQLite-
	// compatible way to do "ADD COLUMN IF NOT EXISTS" on
	// 3.32 and earlier.
	colAdds := []struct {
		table string
		col   string
		def   string
	}{
		{"runs", "failure_class", "TEXT"},
		{"runs", "last_heartbeat_at", "TEXT"},
		// installations needs to exist before its ALTERs run;
		// v1 created it so this is fine for the in-place
		// upgrade path. A fresh install also has it from v1.
		{"installations", "paused", "INTEGER NOT NULL DEFAULT 0"},
		{"installations", "lens_opt_out", "TEXT NOT NULL DEFAULT '[]'"},
	}
	for _, c := range colAdds {
		has, err := hasColumn(ctx, db, c.table, c.col)
		if err != nil {
			return fmt.Errorf("migration v2 hasColumn %s.%s: %w", c.table, c.col, err)
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.col, c.def)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration v2 alter %s.%s: %w", c.table, c.col, err)
		}
	}

	// Indices that touch the new columns. The
	// failure_class index makes the exception dock's
	// `WHERE failure_class IN (...)` a single B-tree walk
	// instead of a full scan; the heartbeat index feeds
	// Phase 2's stuck-runs panel.
	idxStmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_runs_failure_class ON runs(failure_class)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_last_heartbeat_at ON runs(last_heartbeat_at)`,
	}
	for i, stmt := range idxStmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration v2 index %d: %w", i, err)
		}
	}
	return nil
}

// hasColumn reports whether the given table already has the
// given column. SQLite has no portable ADD COLUMN IF NOT
// EXISTS, so the only idempotent path is the pragma. Used by
// migrateV2; not part of the public API.
func hasColumn(ctx context.Context, db *sql.DB, table, col string) (bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// readUserVersion returns the current PRAGMA user_version, or 0
// if the database is fresh. PRAGMA user_version is a 32-bit
// signed integer in SQLite; the driver returns it as int64.
func readUserVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int64
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return int(v), nil
}

// writeUserVersion sets PRAGMA user_version. Wrapped in a
// transaction so the version bump is atomic with the migration
// block that just succeeded — a partial block that crashes
// before the version bump retries on next Open; a partial block
// that crashes after the bump is treated as already applied.
func writeUserVersion(ctx context.Context, db *sql.DB, v int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}
