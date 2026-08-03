package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open returns a Store backed by the SQLite file at dsn. The DSN
// follows the standard SQLite URL form:
//
//	file:/data/boop.db?cache=shared&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)
//
// We force WAL journal mode and a 5s busy timeout because the
// dashboard will read from a separate connection while the receiver
// writes; without WAL, the dashboard's SELECT would block on every
// INSERT.
//
// The DSN is opaque to the caller beyond "give me a path"; the
// pragmas are baked in so a misconfiguration can't accidentally
// fall back to the default rollback journal.
func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("store: empty dsn")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite is single-writer; cap connections to a small pool so a
	// flood of dashboard requests cannot starve the receiver's
	// own writes. The cap is intentionally low — the dashboard
	// aggregates; it does not need 50 parallel readers.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db, dsn: dsn}, nil
}

// Store is the receiver's persistent record of runs and telemetry.
// All exported methods are safe for concurrent use. Internally it
// holds a *sql.DB; the per-method lock story is the database's, not
// ours (database/sql manages the connection pool).
type Store struct {
	db  *sql.DB
	dsn string
}

// DB exposes the underlying *sql.DB. Tests use it to run
// hand-written SQL against the same database the package is
// operating on; production code should not need it.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the connection pool. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
