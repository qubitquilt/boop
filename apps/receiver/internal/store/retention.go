package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Retention knobs. The receiver's webhook handler treats 0 as
// "use the default"; the store package is the source of truth
// for the defaults so a future change does not have to touch
// every call site. The values match the issue spec: 365 days
// retention, 5 min cleanup tick, 7 day vacuum interval.
const (
	// DefaultRetention is how far back PruneRuns keeps a run
	// before deleting it. 365 days is a year; long enough that
	// a slow repo's "oldest review" still shows up, short
	// enough that the dashboard's "this year" view does not
	// grow unbounded. Tunable per-deployment via env
	// (DB_RETENTION).
	DefaultRetention = 365 * 24 * time.Hour

	// DefaultCleanupEvery is the cadence at which the
	// receiver's retention loop calls RunRetention. The loop
	// is best-effort: a transient error is logged and the
	// next tick retries.
	DefaultCleanupEvery = 5 * time.Minute

	// DefaultVacuumInterval is the minimum time between
	// PRAGMA incremental_vacuum calls. The cleanup tick
	// runs every 5 min but only fires the vacuum when this
	// interval has elapsed since the last one. The 7-day
	// default is conservative — incremental_vacuum is
	// non-blocking but does add I/O on the data path.
	DefaultVacuumInterval = 7 * 24 * time.Hour
)

// RetentionResult is the outcome of a single RunRetention
// pass. Returned to the caller (and logged) so an operator can
// see the shape of a tick without grepping the log for the
// individual statements.
type RetentionResult struct {
	Pruned      int64         // rows deleted by PruneRuns
	WALCheck    bool          // true if PRAGMA wal_checkpoint(TRUNCATE) ran
	Vacuumed    bool          // true if PRAGMA incremental_vacuum ran this tick
	VacuumPages int64         // pages returned to the OS by incremental_vacuum
	DurationMS  int64         // wall-clock time for the whole tick
}

// lastVacuumMu guards lastVacuumAt; the retention tick can run
// concurrently with a /health?deep=1 (Stats reads freelist_count
// while the vacuum is mid-flight). The mutex is held only for
// the time.Time read/write, not for the vacuum itself.
var lastVacuumMu sync.Mutex

// lastVacuumAt records the last time the store ran
// incremental_vacuum. Starts as the zero value; the first
// RunRetention call skips the vacuum (since we don't know how
// long it's been since the previous one ran) and just records
// the current time. The second call, after VacuumInterval has
// elapsed, actually runs the vacuum.
//
// In-memory only: a receiver restart resets the clock. The
// effect is that the first vacuum after a restart is delayed
// by VacuumInterval, not by "time since last vacuum" (which is
// unknown). This is the safe default — running an
// incremental_vacuum at startup is wasteful and can race with
// an in-flight web request.
var lastVacuumAt time.Time

// PruneRuns deletes runs whose started_at is older than
// retention-before-now. Telemetry rows are removed via the
// ON DELETE CASCADE on telemetry.run_id, so the foreign_keys
// pragma must be on (it is, see store_db.go buildDSN).
//
// Returns the number of runs deleted. The caller (the
// retention loop) does not need to act on the count, but it is
// logged so a quiet PruneRuns is visible in the receiver log.
//
// A retention of 0 or negative deletes every run. The
// retention loop is responsible for clamping to a sane value
// (see StartRetentionLoop in the webhook package); PruneRuns
// itself honors the input verbatim so a unit test can pass 0
// and verify "delete everything."
func (s *Store) PruneRuns(ctx context.Context, retention time.Duration) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: nil")
	}
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RunRetention is the periodic tick called by the receiver's
// retention loop. It does three things in order:
//
//  1. PruneRuns: delete runs older than `retention`. The
//     ON DELETE CASCADE on telemetry takes care of the child
//     rows.
//  2. PRAGMA wal_checkpoint(TRUNCATE): checkpoint the WAL
//     into the main database file and truncate the WAL. The
//     default wal_autocheckpoint is 1000 pages, which can leave
//     a multi-MB WAL sidecar between the auto-checkpoints;
//     this explicit TRUNCATE is what keeps the sidecar small.
//  3. PRAGMA incremental_vacuum: if VacuumInterval has elapsed
//     since the last vacuum, reclaim freelist pages back to
//     the OS. With auto_vacuum=INCREMENTAL, the freelist grows
//     on every DELETE; without this step the on-disk file
//     never shrinks.
//
// The function is safe to call from a single goroutine. The
// returned error is the first failure encountered; the caller
// should log it and let the next tick retry. Each step is
// logged at Info level on success so the loop's behavior is
// visible in the receiver log without grepping for the
// individual statements.
func (s *Store) RunRetention(ctx context.Context, retention, vacuumInterval time.Duration) (RetentionResult, error) {
	if s == nil || s.db == nil {
		return RetentionResult{}, fmt.Errorf("store: nil")
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	if vacuumInterval <= 0 {
		vacuumInterval = DefaultVacuumInterval
	}
	start := time.Now()
	var res RetentionResult

	// 1. Prune. A 0 retention means "delete everything" and is
	// a fine test value, but we want to avoid an accidental
	// deployment that wipes the DB. Clamp to a 1-hour floor.
	if retention < time.Hour {
		retention = time.Hour
	}
	pruned, err := s.PruneRuns(ctx, retention)
	if err != nil {
		return res, err
	}
	res.Pruned = pruned

	// 2. WAL checkpoint. PRAGMA wal_checkpoint(TRUNCATE) is
	// idempotent and cheap when the WAL is already empty.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return res, fmt.Errorf("store: wal_checkpoint: %w", err)
	}
	res.WALCheck = true

	// 3. Incremental vacuum, gated on the interval.
	lastVacuumMu.Lock()
	last := lastVacuumAt
	now := time.Now()
	shouldVacuum := !last.IsZero() && now.Sub(last) >= vacuumInterval
	lastVacuumMu.Unlock()
	if shouldVacuum {
		// PRAGMA incremental_vacuum with no N argument runs
		// the full vacuum. The pragma returns the number of
		// pages it managed to reclaim; we log that for
		// visibility.
		vacuumPages, err := s.incrementalVacuum(ctx)
		if err != nil {
			return res, err
		}
		res.Vacuumed = true
		res.VacuumPages = vacuumPages
		lastVacuumMu.Lock()
		lastVacuumAt = now
		lastVacuumMu.Unlock()
	} else {
		// First tick after startup: stamp the clock so the
		// next tick (after vacuumInterval) actually fires.
		// Without this, lastVacuumAt would stay zero and the
		// "since last" calculation would keep returning 0,
		// which means "always vacuum" (the opposite of what
		// we want on the first tick).
		lastVacuumMu.Lock()
		if lastVacuumAt.IsZero() {
			lastVacuumAt = now
		}
		lastVacuumMu.Unlock()
	}

	res.DurationMS = time.Since(start).Milliseconds()
	slog.Info("store: retention tick",
		"pruned", res.Pruned,
		"wal_check", res.WALCheck,
		"vacuumed", res.Vacuumed,
		"vacuum_pages", res.VacuumPages,
		"duration_ms", res.DurationMS,
	)
	return res, nil
}

// incrementalVacuum runs PRAGMA incremental_vacuum and returns
// the number of pages it reclaimed. The pragma is non-blocking
// (it does not hold an exclusive lock for the whole duration
// the way a full VACUUM does), so it is safe to call from the
// receiver's periodic loop.
//
// A return of 0 is normal: the retention loop is called every
// 5 min, and a quiet 5 min after a vacuum yields 0 pages. The
// loop logs the count so an operator can see the trend.
func (s *Store) incrementalVacuum(ctx context.Context) (int64, error) {
	var pages int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA incremental_vacuum`).Scan(&pages); err != nil {
		return 0, fmt.Errorf("store: incremental_vacuum: %w", err)
	}
	return pages, nil
}
