package dashboard

import (
	"context"
	"database/sql"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// Store is the read+write surface the dashboard needs
// from the persistent layer. SP-005: a typed interface
// here means (a) tests can mock the data layer without
// touching a real SQLite file, (b) a future dashboard
// pointed at a Postgres / REST / mock backend is one
// call away (the interface is the seam), and (c) the
// Handler struct no longer carries a *sql.DB through
// its dependency chain.
//
// The interface lists every method the dashboard's
// view handlers call. It is intentionally narrow —
// methods the dashboard does not use (RecordTelemetry,
// etc.) are not in the surface. A method the dashboard
// adds is one line on the interface and one method on
// the test double. *store.Store implements this
// interface transparently.
type Store interface {
	GetRun(ctx context.Context, id string) (store.Run, error)
	ListRuns(ctx context.Context, f store.ListRunsFilter) (store.ListRunsResult, error)
	ListRunsWithTelemetry(ctx context.Context, f store.ListRunsFilter) (store.ListRunsWithTelemetryPage, error)
	ListStuckRuns(ctx context.Context, olderThan time.Duration, limit int) ([]store.Run, error)
	GetTelemetry(ctx context.Context, runID string) (store.Telemetry, error)
	ListRunStages(ctx context.Context, runID string) ([]store.RunStage, error)
	ListLensTelemetry(ctx context.Context, runID string) ([]store.LensTelemetry, error)
	WalkLineage(ctx context.Context, start string, maxDepth int) (store.Lineage, error)
	ListInstallations(ctx context.Context) ([]store.Installation, error)
	GetInstallation(ctx context.Context, id int64) (store.Installation, error)
	SetInstallationControls(ctx context.Context, id int64, paused bool, lensOptOut []string) error
	RecordAuditEvent(ctx context.Context, ev store.AuditEvent) (store.AuditEvent, error)
	ListAuditEvents(ctx context.Context, limit int) ([]store.AuditEvent, error)
	ListRetentionSchedule(ctx context.Context, retention time.Duration) ([]store.RetentionRow, error)
	LensCostSummary(ctx context.Context, from, to time.Time) ([]store.LensCostRollup, error)
	RecordRefund(ctx context.Context, r store.Refund) (store.Refund, error)
	ListRefunds(ctx context.Context, runID string) ([]store.Refund, error)
	MarkOrphanedRuns(ctx context.Context, grace time.Duration) (int64, error)
	Stats(ctx context.Context) (store.StoreStats, error)

	// Seeders used by the dashboard's tests. The
	// production wiring never calls these (the
	// webhook handler populates the runs table);
	// the tests use them to set up a scenario.
	// They are part of the Store interface so a
	// test double has to implement them too —
	// otherwise a future change to the seeders
	// would silently pass a missing mock method
	// and the test would compile but fail at
	// runtime.
	UpsertRun(ctx context.Context, r store.Run) (store.Run, error)
	UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, endedAt *time.Time, durationMS *int64, errMsg string) (store.Run, error)
	UpsertInstallations(ctx context.Context, installs []store.Installation) error
	RecordTelemetry(ctx context.Context, t store.Telemetry) error
	TouchRunHeartbeat(ctx context.Context, runID string) error
}

// TestStore is the seam for tests that need direct
// *sql.DB access (raw SQL UPDATEs for backdating
// timestamps, etc.). *store.Store implements it
// transparently. A test double that does not need
// raw SQL can simply leave DB() returning nil.
type TestStore interface {
	Store
	DB() *sql.DB
}

// compile-time check: *store.Store must implement
// both Store and TestStore.
var _ Store = (*store.Store)(nil)
var _ TestStore = (*store.Store)(nil)
