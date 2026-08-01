package store

import (
	"database/sql"
	"fmt"
)

// applyMigrations creates the schema if it does not exist. The
// schema is intentionally simple: two tables, a handful of indices,
// and no ORM. Migration history is not kept on disk because the
// schema has no versioning to speak of (this is the first version)
// — if the schema needs to change, add a new CREATE/ALTER here and
// gate it on a feature check (e.g. PRAGMA user_version).
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
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}
	return nil
}
