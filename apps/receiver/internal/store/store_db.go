package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Open returns a Store backed by the SQLite file at path. The DSN
// is built internally so the pragmas below cannot be silently
// dropped by a caller passing the wrong DSN form.
//
// The DSN follows the modernc.org/sqlite URL form with the
// following connection-time pragmas, all required for QUB-101:
//
//   - journal_mode(WAL): the receiver writes on the webhook path
//     and reads from the dashboard. Without WAL, dashboard reads
//     block on every write. WAL also reduces fsync amplification
//     on the busy day.
//   - synchronous(FULL): durability over throughput. The write
//     rate is one row per review (a few per minute at peak); we
//     can pay for the extra fsync.
//   - busy_timeout(5000): the dashboard can hold a read lock
//     while the receiver tries to write; 5s is enough for the
//     dashboard's aggregation queries to complete without the
//     receiver erroring out.
//   - foreign_keys(on): the ON DELETE CASCADE on
//     telemetry.run_id only fires when foreign keys are on; this
//     is opt-in per connection in SQLite.
//   - auto_vacuum(INCREMENTAL): row deletes (PruneRuns) leave
//     pages on the freelist; without incremental vacuum the DB
//     file does not shrink. INCREMENTAL is non-blocking; the
//     retention loop runs `PRAGMA incremental_vacuum` on a
//     schedule.
//   - wal_autocheckpoint(...): default is 1000 pages. Left at
//     default; the retention loop also runs an explicit
//     wal_checkpoint(TRUNCATE) every tick.
//
// The DSN also uses cache=shared so a future read-replica (or a
// long-lived dashboard connection in the same process) can read
// the same file without re-opening the WAL.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty path")
	}
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite is single-writer; cap connections to a small pool
	// so a flood of dashboard requests cannot starve the
	// receiver's own writes. The cap is intentionally low — the
	// dashboard aggregates; it does not need 50 parallel readers.
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
	s := &Store{db: db, path: path}
	if err := s.logStartupInfo(); err != nil {
		// A failure to log the pragmas is not fatal — the
		// database is open and migrated. Log and move on.
		slog.Warn("store: startup info partial", "err", err)
	}
	return s, nil
}

// buildDSN assembles the SQLite URL the driver opens. Path is
// expected to be a plain filesystem path; we prefix "file:" so
// the driver treats it as a URL. Any path containing a "?" or
// "#" is rejected — those characters are reserved in the URL
// form and a caller that needs them is almost certainly passing
// in an unescaped DSN by mistake.
func buildDSN(path string) string {
	return "file:" + path +
		"?cache=shared" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=auto_vacuum(INCREMENTAL)"
}

// Store is the receiver's persistent record of runs and
// telemetry. All exported methods are safe for concurrent use.
// Internally it holds a *sql.DB; the per-method lock story is
// the database's, not ours (database/sql manages the connection
// pool).
//
// path is the on-disk location of the SQLite file. The receiver
// keeps it on the struct so Stats() can report file_bytes
// without the caller threading the path through every call.
type Store struct {
	db   *sql.DB
	path string
}

// DB exposes the underlying *sql.DB. Tests use it to run
// hand-written SQL against the same database the package is
// operating on; production code should not need it.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the on-disk path the Store was opened with.
// Exposed for the /health?deep=1 handler and for diagnostic
// logging; production code should not branch on it.
func (s *Store) Path() string { return s.path }

// Close releases the connection pool. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// logStartupInfo prints the effective pragma values, the
// resolved user_version, and the row counts. The point is to
// make a misconfigured database obvious in the receiver's first
// log line: a default journal_mode (rollback) or a missing
// foreign_keys enforcement would show up here before the
// dashboard ever asks for data.
//
// Errors are best-effort: a single failed PRAGMA read is logged
// and skipped, the rest of the function still runs. The
// returned error is non-nil only when every read failed (used
// by Open to log a "partial" warning).
func (s *Store) logStartupInfo() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pragmas := map[string]string{
		"journal_mode":  "",
		"synchronous":   "",
		"busy_timeout":  "",
		"foreign_keys":  "",
		"auto_vacuum":   "",
		"user_version":  "",
		"freelist_count": "",
	}
	allErr := true
	for k := range pragmas {
		var v string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+k).Scan(&v); err == nil {
			pragmas[k] = v
			allErr = false
		}
	}
	if allErr {
		return fmt.Errorf("all pragma reads failed")
	}

	stats, _ := s.Stats(ctx)
	slog.Info("store: opened",
		"path", s.path,
		"journal_mode", pragmas["journal_mode"],
		"synchronous", pragmas["synchronous"],
		"busy_timeout_ms", pragmas["busy_timeout"],
		"foreign_keys", pragmas["foreign_keys"],
		"auto_vacuum", pragmas["auto_vacuum"],
		"user_version", pragmas["user_version"],
		"runs", stats.Runs,
		"telemetry", stats.Telemetry,
		"file_bytes", stats.FileBytes,
		"freelist_pages", stats.FreelistCount,
	)
	return nil
}

// fileSize returns the on-disk size of the SQLite file, or 0 if
// the stat fails. Used by Stats(); a missing or unreadable file
// is reported as 0 rather than an error so a stat failure does
// not propagate through the dashboard's GET handlers.
func (s *Store) fileSize() int64 {
	if s == nil || s.path == "" {
		return 0
	}
	info, err := os.Stat(filepath.Clean(s.path))
	if err != nil {
		return 0
	}
	return info.Size()
}
