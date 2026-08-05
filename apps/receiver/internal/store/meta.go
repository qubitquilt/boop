package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// StoreStats is the metadata the /health?deep=1 handler reports
// about the database itself, as opposed to the dashboard's
// business-level rollups (SummaryStats). The two are deliberately
// separate: a slow drift in the file size or the freelist is
// invisible from a SELECT against runs/telemetry, and the
// operator needs to see both shapes side by side.
type StoreStats struct {
	// Runs is the row count in the runs table. Drift here
	// indicates a retention loop that's not running (rows
	// accumulate forever) or a PruneRuns that's over-aggressive
	// (rows disappear too fast).
	Runs int64
	// Telemetry is the row count in the telemetry table. The
	// dashboard joins the two; the ratio Telemetry/Runs is
	// normally ~1.0 for recent runs and falls off as older
	// runs age out (telemetry is deleted via the FK cascade
	// when the parent run is pruned).
	Telemetry int64
	// FileBytes is the on-disk size of the SQLite file. Compare
	// against the previous /health?deep=1 response to detect a
	// runaway growth that the retention loop has not caught up
	// with yet.
	FileBytes int64
	// FreelistCount is the number of pages SQLite has on its
	// freelist. With auto_vacuum=INCREMENTAL, the file does
	// not shrink until incremental_vacuum runs; a freelist
	// that grows monotonically indicates the incremental
	// vacuum is not keeping up.
	FreelistCount int64
}

// Stats returns the database's metadata in a single struct.
// All four numbers come from independent SQLite calls; the
// function is best-effort: a single failed read returns 0 for
// that field rather than aborting the call, so a partial read
// still produces a useful /health?deep=1 body. Errors are
// logged at Warn level so an operator can correlate an
// unexpected zero in the response with the corresponding log.
//
// The context is honored; a slow disk shows up as a slow
// /health?deep=1, not as a hung probe.
func (s *Store) Stats(ctx context.Context) (StoreStats, error) {
	var out StoreStats
	if s == nil || s.db == nil {
		return out, fmt.Errorf("store: nil")
	}

	read := func(q string, dst *int64) {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var v int64
		if err := s.db.QueryRowContext(c, q).Scan(&v); err != nil {
			slog.Warn("store: stats read failed", "q", q, "err", err)
			return
		}
		*dst = v
	}
	read(`SELECT COUNT(*) FROM runs`, &out.Runs)
	read(`SELECT COUNT(*) FROM telemetry`, &out.Telemetry)
	read(`PRAGMA freelist_count`, &out.FreelistCount)
	out.FileBytes = s.fileSize()
	return out, nil
}
