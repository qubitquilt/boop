package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RunStatus is the lifecycle state of a single review. The values
// are the same as the K8s Job's humanStatus ("Running", "Complete",
// "Failed") plus "pending" for the moment between webhook accept
// and Job start, and "queued" for a re-delivery that was deduped
// before the runner took over. The strings are stable: the
// dashboard reads them directly.
type RunStatus string

const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
)

// Run is one review. The id is the K8s Job name (e.g.
// "boop-qubitquilt-boop-42-a1b2c3d") so the dashboard can join the
// run row to the live Job for the duration the Job is in the
// namespace. After the Job is GC'd, the id is still meaningful —
// it carries the owner, repo, PR number, and short SHA.
//
// FailureClass and LastHeartbeatAt are QUB-108/QUB-109
// additions: FailureClass is set by the receiver at
// UpsertRun time from the K8s container exit reason (or by
// the runner at end-of-run for non-pod failures like
// json_parse_fail); LastHeartbeatAt is updated by the
// receiver on every runner heartbeat POST and powers the
// stuck-runs panel.
//
// ParentRunID and SupersededByID are QUB-110 additions:
// ParentRunID points to the run this one was re-run from;
// SupersededByID points to the run that re-ran this one.
// The two columns are not symmetric — a re-run never
// branches, so SupersededByID is at most one row. The
// dashboard's "vertical timeline" view walks the chain
// via ParentRunID.
// JSON tags are added so the /api/runs response has stable snake_case
// keys (the CLI and any future API consumer depends on this). Without
// tags Go would emit PascalCase field names, which is not a stable
// wire format. The HTML dashboard is unaffected — it renders via Go
// templates that access struct fields directly, not JSON keys.
type Run struct {
	ID              string     `json:"id"`
	Owner           string     `json:"owner"`
	Repo            string     `json:"repo"`
	PRNumber        int        `json:"pr_number"`
	CommitSHA       string     `json:"commit_sha"`
	BaseRef         string     `json:"base_ref"`
	ReviewNumber    int        `json:"review_number"`
	Reason          string     `json:"reason,omitempty"`
	InstallationID  int64      `json:"installation_id"`
	Status          RunStatus  `json:"status"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	DurationMS      *int64     `json:"duration_ms,omitempty"`
	Error           string     `json:"error,omitempty"`
	FailureClass    string     `json:"failure_class,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	ParentRunID     string     `json:"parent_run_id,omitempty"`
	SupersededByID  string     `json:"superseded_by_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// UpsertRun inserts the run if it does not exist, or updates the
// mutable fields if it does. The PRIMARY KEY is the Job name; a
// re-delivery for the same head SHA hits the same row.
//
// Mutable fields (status, ended_at, duration_ms, error, updated_at)
// are always overwritten. Immutable fields (owner, repo, pr_number,
// commit_sha, base_ref, review_number, reason, installation_id,
// started_at) are left alone on UPDATE — they were captured at
// webhook time and should not drift if a re-delivery races the
// runner's first status update.
//
// Returns the final row (post-merge) so callers can echo it back
// without a second SELECT.
func (s *Store) UpsertRun(ctx context.Context, r Run) (Run, error) {
	if r.ID == "" {
		return Run{}, errors.New("store: UpsertRun: empty id")
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (
			id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class,
			parent_run_id, superseded_by_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status           = excluded.status,
			ended_at         = excluded.ended_at,
			duration_ms      = excluded.duration_ms,
			error            = excluded.error,
			failure_class    = excluded.failure_class,
			parent_run_id    = excluded.parent_run_id,
			superseded_by_id = excluded.superseded_by_id,
			updated_at       = excluded.updated_at
	`,
		r.ID, r.Owner, r.Repo, r.PRNumber, r.CommitSHA, r.BaseRef,
		r.ReviewNumber, nullString(r.Reason), nullInt64(r.InstallationID), string(r.Status),
		r.StartedAt.UTC().Format(time.RFC3339Nano), nullTimePtr(r.EndedAt), nullInt64Ptr(r.DurationMS), nullString(r.Error),
		nullString(r.FailureClass),
		nullString(r.ParentRunID), nullString(r.SupersededByID),
		r.CreatedAt.UTC().Format(time.RFC3339Nano), r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return Run{}, fmt.Errorf("store: upsert run: %w", err)
	}
	return s.GetRun(ctx, r.ID)
}

// UpdateRunStatus is the narrow entry point the runner uses to
// advance a run's status as it works through its stages. It writes
// status + (optionally) ended_at/duration_ms/error in one shot so
// the dashboard sees a single coherent transition, not two writes
// with an observable intermediate state.
//
// The status is the only required field; ended/duration/error are
// only updated when the caller supplies them. Returns the updated
// row, or ErrUnknownRun if the run does not exist (the runner
// started before the receiver committed the row, which is fine —
// the runner will retry on the next stage).
//
// RD-003: this method (and TouchRunHeartbeat) used to return the
// raw sql.ErrNoRows for the missing-row case. The HTTP boundary
// then had three different shapes for "unknown run" — ErrUnknownRun
// for some reads, sql.ErrNoRows for the status/heartbeat
// writes, and the FK-violation detection on the POSTs. Every
// store method that signals "the run does not exist" now
// returns ErrUnknownRun; HTTP handlers match on that single
// shape.
func (s *Store) UpdateRunStatus(ctx context.Context, id string, status RunStatus, endedAt *time.Time, durationMS *int64, errMsg string) (Run, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET
			status      = ?,
			ended_at    = COALESCE(?, ended_at),
			duration_ms = COALESCE(?, duration_ms),
			error       = COALESCE(NULLIF(?, ''), error),
			updated_at  = ?
		WHERE id = ?
	`,
		string(status),
		nullTimePtr(endedAt),
		nullInt64Ptr(durationMS),
		errMsg,
		now.UTC().Format(time.RFC3339Nano),
		id,
	)
	if err != nil {
		return Run{}, fmt.Errorf("store: update run status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, fmt.Errorf("store: update run status rows: %w", err)
	}
	if n == 0 {
		return Run{}, ErrUnknownRun
	}
	return s.GetRun(ctx, id)
}

// UpdateRunStatusIfRunning is the reconciler's variant of
// UpdateRunStatus: it only writes the terminal fields when the
// current row is still "running". Used by the K8s Job
// reconciler so a late reconciler tick cannot overwrite a
// status the runner already finalised via UpdateRunStatus
// (e.g. "succeeded" with a posted summary comment).
//
// Returns (true, nil) when the row was updated, (false, nil)
// when the row was already terminal, (Run{}, sql.ErrNoRows)
// when no such row exists. The bool lets the reconciler
// log "we reconciled N runs" without a follow-up SELECT.
func (s *Store) UpdateRunStatusIfRunning(ctx context.Context, id string, status RunStatus, endedAt *time.Time, durationMS *int64, errMsg string) (bool, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET
			status      = ?,
			ended_at    = COALESCE(?, ended_at),
			duration_ms = COALESCE(?, duration_ms),
			error       = COALESCE(NULLIF(?, ''), error),
			updated_at  = ?
		WHERE id = ? AND status = ?
	`,
		string(status),
		nullTimePtr(endedAt),
		nullInt64Ptr(durationMS),
		errMsg,
		now.UTC().Format(time.RFC3339Nano),
		id,
		string(StatusRunning),
	)
	if err != nil {
		return false, fmt.Errorf("store: update run status if running: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: update run status if running rows: %w", err)
	}
	return n > 0, nil
}

// SetRunFailureClass writes just the failure_class column on a
// run row. Used by the K8s reconciler (Phase 1's post-createJob
// path) to backfill the exit reason once the Job terminates.
// Splitting this from UpdateRunStatus keeps the
// already-witnessed status/ended_at fields from being
// overwritten by a later reconciler tick, and it lets the
// reconciler do an empty-string clear ("no failure class
// known") without coupling that to a status update.
//
// Returns ErrUnknownRun if the run has been pruned between
// the reconciler's read of the Job and the write here — the
// reconciler treats that as a no-op so it doesn't have to
// retry on a retention race. RD-003: returns ErrUnknownRun
// (not sql.ErrNoRows) so the HTTP boundary has a single
// shape for "unknown run".
func (s *Store) SetRunFailureClass(ctx context.Context, id, failureClass string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET failure_class = ?, updated_at = ? WHERE id = ?
	`, nullString(failureClass), now, id)
	if err != nil {
		return fmt.Errorf("store: set run failure class: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set run failure class rows: %w", err)
	}
	if n == 0 {
		return ErrUnknownRun
	}
	return nil
}

// CreateRerun persists a new re-run row and backfills
// the prior's superseded_by_id pointer in a single
// transaction. The re-run flow (webhook/rerun.go:
// CreateRerunJob) used to do the two writes as separate
// statements; a crash between them would leave a new
// pending row with no lineage pointer or, worse, a prior
// row that already points at a non-existent new id. The
// dashboard's "Lineage" view (run_detail.html) reads
// both, so a partial state is a silent corruption: the
// "WalkDown" pill on the prior row would 404.
//
// The next.ID is expected to be the candidate name; the
// caller computes it from CountRerunJobsForSHA + 1.
// The INSERT runs first; if it lands, the UPDATE
// supersedes the prior. The single-transaction shape
// guarantees both writes land together or neither does.
//
// EH-008: the prior shape had a TOCTOU race between
// CountRerunJobsForSHA (read) and the INSERT — two
// concurrent re-runs of the same prior would both read
// count=0, both try to insert "X-r1", and the second
// would either silently overwrite (ON CONFLICT) or fail.
// We close the race by removing the ON CONFLICT from
// the INSERT and returning ErrDuplicateRerunName when
// SQLite's UNIQUE constraint fires. The caller
// (CreateRerunJob) retries with count+1; after a
// bounded number of attempts the race is so unlikely
// that an error is the right answer.
//
// Returns the persisted new row (post-merge, same shape
// UpsertRun returns). Returns sql.ErrNoRows if the prior
// row does not exist — the caller can treat that as
// "retention pruned the prior between the operator's
// click and the write", log it, and proceed without
// lineage (the new row's parent_run_id is still set so
// the up-chain view works). Returns the prior's id
// unchanged when the prior was already superseded
// (UPDATE rows=0 from the second write), so a double-
// click on the operator's "Requeue" button is a no-op
// rather than a clobber.
func (s *Store) CreateRerun(ctx context.Context, next Run, priorID string) (Run, error) {
	if next.ID == "" {
		return Run{}, errors.New("store: CreateRerun: empty id")
	}
	if priorID == "" {
		return Run{}, errors.New("store: CreateRerun: empty prior id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("store: begin rerun tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	next.UpdatedAt = now

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runs (
			id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class,
			parent_run_id, superseded_by_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		next.ID, next.Owner, next.Repo, next.PRNumber, next.CommitSHA, next.BaseRef,
		next.ReviewNumber, nullString(next.Reason), nullInt64(next.InstallationID), string(next.Status),
		next.StartedAt.UTC().Format(time.RFC3339Nano), nullTimePtr(next.EndedAt), nullInt64Ptr(next.DurationMS), nullString(next.Error),
		nullString(next.FailureClass),
		nullString(next.ParentRunID), nullString(next.SupersededByID),
		next.CreatedAt.UTC().Format(time.RFC3339Nano), next.UpdatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		if isUniqueConstraintError(err) {
			// EH-008: a concurrent re-run got the
			// same candidate name. The caller
			// retries with count+1.
			return Run{}, ErrDuplicateRerunName
		}
		return Run{}, fmt.Errorf("store: insert rerun: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET superseded_by_id = ?, updated_at = ?
		WHERE id = ? AND (superseded_by_id IS NULL OR superseded_by_id = '' OR superseded_by_id = ?)
	`, nullString(next.ID), now.UTC().Format(time.RFC3339Nano), priorID, next.ID)
	if err != nil {
		return Run{}, fmt.Errorf("store: supersede prior: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Run{}, fmt.Errorf("store: supersede prior rows: %w", err)
	} else if n == 0 {
		// Prior was either pruned or already pointed
		// at this re-run. Read it inside the same tx
		// so we can distinguish the two cases for the
		// caller.
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE id = ?`, priorID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, sql.ErrNoRows
		}
		if err != nil {
			return Run{}, fmt.Errorf("store: prior probe: %w", err)
		}
		// Prior exists with a different superseded_by_id.
		// Idempotent no-op: the second call sees the
		// already-set pointer and skips the write.
	}

	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("store: commit rerun: %w", err)
	}
	return s.GetRun(ctx, next.ID)
}

// ErrDuplicateRerunName is returned by CreateRerun when
// the candidate name (computed from a stale
// CountRerunJobsForSHA) collides with a row that landed
// in the gap between the count and the INSERT. The
// caller (webhook.CreateRerunJob) is expected to retry
// with a count+1 candidate. EH-008 closes the
// count-then-insert race that the previous shape
// silently lost to.
var ErrDuplicateRerunName = errors.New("store: duplicate rerun name (concurrent re-run)")

// isUniqueConstraintError reports whether err is a
// SQLite UNIQUE constraint violation. The error
// message is the only stable signal; matching the
// substring is the same approach the rest of the store
// uses for FK violations.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// SetSupersededBy writes just the superseded_by_id column
// on a run row. Used by the re-run flow (Phase 3) to
// backfill the lineage pointer without re-touching any
// other field. The prior run's status, ended_at, error,
// failure_class, etc. stay exactly as the runner left
// them; only the new lineage pointer lands.
//
// Returns sql.ErrNoRows if the prior has been pruned —
// the re-run flow treats that as a non-fatal no-op so
// the new run row still lands and the operator's
// "show me the lineage" view is consistent (the prior
// just renders as "(pruned)" on the dashboard side).
//
// CreateRerun is the preferred entry point for the
// re-run flow; SetSupersededBy is left for tests and
// future narrow callers (e.g. a "link this prior to
// that new id" admin action that does not also insert
// a new row).
func (s *Store) SetSupersededBy(ctx context.Context, priorID, newID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET superseded_by_id = ?, updated_at = ? WHERE id = ?
	`, nullString(newID), now, priorID)
	if err != nil {
		return fmt.Errorf("store: set superseded by: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set superseded by rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkOrphanedRuns bulk-marks every "running" row whose
// runner never heartbeated as "failed" with a synthetic
// "orphaned" error. Used by the dashboard's admin endpoint
// to clean up the dashboard after a deployment regression
// where runner telemetry posts never landed (see QUB-114
// for the original incident): every review was created
// with status="running" but the runner never reached the
// heartbeat stage, so the rows are stuck in the live view
// indefinitely until the 365-day retention tick prunes
// them.
//
// The grace argument skips rows younger than now-grace:
// a healthy review typically takes 30-90s to reach its
// first heartbeat, and the admin endpoint should not race
// with the next in-flight review. 5 minutes is the
// documented floor for the dashboard's "stuck" panel and
// doubles as the orphan grace here — anything older than
// 5m and still heartbeating-free is, by definition, an
// orphan.
//
// Returns the number of rows updated.
func (s *Store) MarkOrphanedRuns(ctx context.Context, grace time.Duration) (int64, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-grace).UTC().Format(time.RFC3339Nano)
	endedAt := now.UTC().Format(time.RFC3339Nano)
	errMsg := "orphaned: K8s Job no longer exists; runner never recorded final status"
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET
			status      = ?,
			ended_at    = ?,
			error       = ?,
			updated_at  = ?
		WHERE status = ?
		  AND last_heartbeat_at IS NULL
		  AND started_at < ?
	`,
		string(StatusFailed),
		endedAt,
		errMsg,
		endedAt,
		string(StatusRunning),
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("store: mark orphaned runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark orphaned runs rows: %w", err)
	}
	return n, nil
}

// GetRun returns the run with the given id, or ErrUnknownRun
// (wrapping sql.ErrNoRows) if none exists. Used by the
// runner's POST endpoints to look up the row before they
// update it.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class, last_heartbeat_at,
			COALESCE(parent_run_id, ''), COALESCE(superseded_by_id, ''),
			created_at, updated_at
		FROM runs WHERE id = ?
	`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrUnknownRun
	}
	return r, err
}

// ListRunsFilter narrows a ListRuns call. Empty fields are ignored
// (no constraint). From/To are inclusive bounds on started_at; if
// From is zero, no lower bound; if To is zero, no upper bound. The
// cursor is the (started_at, id) of the last item in the previous
// page, or empty for the first page. Limit is clamped to 1..200.
type ListRunsFilter struct {
	Owner          string
	Repo           string
	Status         RunStatus
	FailureClass   string
	From           time.Time
	To             time.Time
	InstallationID int64
	Cursor         string
	Limit          int
}

// ListRunsResult is the page returned by ListRuns.
type ListRunsResult struct {
	Runs       []Run
	NextCursor string
}

// RunWithTelemetry is one run joined with its telemetry row.
// The two live on different tables (runs.id →
// telemetry.run_id) but every dashboard run-list cell
// renders them as one row, so the join is the load-bearing
// shape for the dashboard's data layer.
//
// SP-006: ListRunsWithTelemetry is the bulk version. The
// prior shape was "ListRuns + N×GetTelemetry" — N+1 to
// the database, the same shape the audit flagged. This
// helper does one runs query + one telemetry batch so
// the dashboard can render a 200-row run list in two
// round-trips rather than 201.
type RunWithTelemetry struct {
	Run       Run
	Telemetry Telemetry
}

// ListRunsWithTelemetryPage is the page variant — it
// returns the bulk shape keyed with the next-cursor from
// the runs side so callers that paginate can keep the
// cursor flowing.
type ListRunsWithTelemetryPage struct {
	Runs       []RunWithTelemetry
	NextCursor string
}

// ListRunsWithTelemetry returns the same page ListRuns
// would, with each row carrying its telemetry fields
// (zero-valued when no telemetry row exists for the run).
// Two round-trips: one for the runs page, one for the
// matching telemetry rows. The cursor is preserved so a
// paginated caller can chain calls.
//
// The store-side test TestListRunsWithTelemetry pins the
// join shape.
func (s *Store) ListRunsWithTelemetry(ctx context.Context, f ListRunsFilter) (ListRunsWithTelemetryPage, error) {
	runsPage, err := s.ListRuns(ctx, f)
	if err != nil {
		return ListRunsWithTelemetryPage{}, err
	}
	if len(runsPage.Runs) == 0 {
		return ListRunsWithTelemetryPage{NextCursor: runsPage.NextCursor}, nil
	}
	ids := make([]any, 0, len(runsPage.Runs))
	placeholders := make([]string, 0, len(runsPage.Runs))
	for _, run := range runsPage.Runs {
		ids = append(ids, run.ID)
		placeholders = append(placeholders, "?")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, model, provider,
			input_tokens, output_tokens, total_tokens,
			reasoning_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, cost_prompt_usd, cost_completion_usd, cost_upstream_usd,
			is_byok,
			server_tool_calls_executed, server_tool_calls_requested,
			request_id, duration_ms, step_count,
			error, error_status_code, error_content_type, error_body
		FROM telemetry WHERE run_id IN (`+strings.Join(placeholders, ",")+`)`,
		ids...)
	if err != nil {
		return ListRunsWithTelemetryPage{}, fmt.Errorf("store: list telemetry batch: %w", err)
	}
	defer rows.Close()
	telemByRun := make(map[string]Telemetry, len(runsPage.Runs))
	for rows.Next() {
		var t Telemetry
		if err := scanTelemetryInto(&t, rows); err != nil {
			return ListRunsWithTelemetryPage{}, fmt.Errorf("store: scan telemetry: %w", err)
		}
		telemByRun[t.RunID] = t
	}
	if err := rows.Err(); err != nil {
		return ListRunsWithTelemetryPage{}, fmt.Errorf("store: telemetry rows: %w", err)
	}
	out := make([]RunWithTelemetry, 0, len(runsPage.Runs))
	for _, run := range runsPage.Runs {
		out = append(out, RunWithTelemetry{
			Run:       run,
			Telemetry: telemByRun[run.ID],
		})
	}
	return ListRunsWithTelemetryPage{Runs: out, NextCursor: runsPage.NextCursor}, nil
}

// scanTelemetryInto fills a *Telemetry from a row that
// already has the run_id as the first column. The shape
// matches the SELECT above.
func scanTelemetryInto(t *Telemetry, r rowScanner) error {
	var (
		errPtr     sql.NullString
		contType   sql.NullString
		body       sql.NullString
		requestID  sql.NullString
		dur        sql.NullInt64
		errCode    sql.NullInt64
		provider   sql.NullString
	)
	if err := r.Scan(
		&t.RunID, &t.Model, &provider,
		&t.InputTokens, &t.OutputTokens, &t.TotalTokens,
		&t.ReasoningTokens, &t.CacheReadTokens, &t.CacheWriteTokens,
		&t.CostUSD, &t.CostPromptUSD, &t.CostCompletionUSD, &t.CostUpstreamUSD,
		&t.IsByok,
		&t.ServerToolCallsExec, &t.ServerToolCallsReq,
		&requestID, &dur, &t.StepCount,
		&errPtr, &errCode, &contType, &body,
	); err != nil {
		return err
	}
	t.Provider = provider.String
	if requestID.Valid {
		v := requestID.String
		t.RequestID = &v
	}
	if dur.Valid {
		v := dur.Int64
		t.DurationMS = &v
	}
	if errPtr.Valid {
		v := errPtr.String
		t.Error = &v
	}
	if errCode.Valid {
		v := errCode.Int64
		t.ErrorStatusCode = &v
	}
	if contType.Valid {
		v := contType.String
		t.ErrorContentType = &v
	}
	if body.Valid {
		v := body.String
		t.ErrorBody = &v
	}
	return nil
}

// ListRuns returns runs ordered newest-first by (started_at, id).
// Pagination is keyset-style on (started_at DESC, id DESC): the
// cursor encodes the boundary, and the SQL adds a `started_at < ?`
// (or `started_at = ? AND id < ?`) WHERE clause. Keyset is the
// right choice here because the run table is append-mostly and the
// "rows per page" shape matches the dashboard's infinite-scroll.
//
// The dashboard never paginates more than 200 rows at a time;
// arbitrary deep history is the stats endpoint's job.
func (s *Store) ListRuns(ctx context.Context, f ListRunsFilter) (ListRunsResult, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var (
		conds []string
		args  []any
	)
	if f.Owner != "" {
		conds = append(conds, "owner = ?")
		args = append(args, f.Owner)
	}
	if f.Repo != "" {
		conds = append(conds, "repo = ?")
		args = append(args, f.Repo)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.FailureClass != "" {
		conds = append(conds, "failure_class = ?")
		args = append(args, f.FailureClass)
	}
	if f.InstallationID != 0 {
		conds = append(conds, "installation_id = ?")
		args = append(args, f.InstallationID)
	}
	if !f.From.IsZero() {
		conds = append(conds, "started_at >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339Nano))
	}
	if !f.To.IsZero() {
		conds = append(conds, "started_at <= ?")
		args = append(args, f.To.UTC().Format(time.RFC3339Nano))
	}
	if f.Cursor != "" {
		cs, id, ok := decodeCursor(f.Cursor)
		if !ok {
			return ListRunsResult{}, fmt.Errorf("store: malformed cursor")
		}
		// Format the cursor's time to the same RFC3339Nano string
		// the rows were written with; otherwise the driver would
		// pick its own default format and the comparison would
		// miss every row.
		csStr := cs.UTC().Format(time.RFC3339Nano)
		conds = append(conds, "(started_at < ? OR (started_at = ? AND id < ?))")
		args = append(args, csStr, csStr, id)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// limit+1 lets us tell whether another page exists without a
	// second COUNT query.
	q := fmt.Sprintf(`
		SELECT id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class, last_heartbeat_at,
			COALESCE(parent_run_id, ''), COALESCE(superseded_by_id, ''),
			created_at, updated_at
		FROM runs
		%s
		ORDER BY started_at DESC, id DESC
		LIMIT ?
	`, where)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ListRunsResult{}, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return ListRunsResult{}, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return ListRunsResult{}, fmt.Errorf("store: rows: %w", err)
	}

	next := ""
	if len(out) > limit {
		last := out[limit-1]
		next = encodeCursor(last.StartedAt, last.ID)
		out = out[:limit]
	}
	return ListRunsResult{Runs: out, NextCursor: next}, nil
}

// encodeCursor / decodeCursor are the boundary format for keyset
// pagination. The cursor is "<rfc3339nano>|<id>" — a single pipe
// so a malformed cursor is easy to detect. We use pipe instead of
// a JSON envelope because the only thing that ever decodes a
// cursor is this package; the wire shape is an implementation
// detail.
func encodeCursor(t time.Time, id string) string {
	return t.UTC().Format(time.RFC3339Nano) + "|" + id
}

func decodeCursor(c string) (time.Time, string, bool) {
	idx := strings.IndexByte(c, '|')
	if idx <= 0 || idx == len(c)-1 {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, c[:idx])
	if err != nil {
		return time.Time{}, "", false
	}
	return t, c[idx+1:], true
}

// rowScanner is the common shape for sql.Row and sql.Rows so the
// same scan function works for both.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(r rowScanner) (Run, error) {
	var (
		rr           Run
		status       string
		startedStr   string
		endedPtr     sql.NullString
		durPtr       sql.NullInt64
		errPtr       sql.NullString
		baseRef      sql.NullString
		reason       sql.NullString
		installID    sql.NullInt64
		failureClass sql.NullString
		heartbeatPtr sql.NullString
		parentID     string
		supersededBy string
		createdStr   string
		updatedStr   string
	)
	if err := r.Scan(
		&rr.ID, &rr.Owner, &rr.Repo, &rr.PRNumber, &rr.CommitSHA, &baseRef,
		&rr.ReviewNumber, &reason, &installID, &status,
		&startedStr, &endedPtr, &durPtr, &errPtr,
		&failureClass, &heartbeatPtr,
		&parentID, &supersededBy,
		&createdStr, &updatedStr,
	); err != nil {
		return Run{}, err
	}
	rr.Status = RunStatus(status)
	rr.BaseRef = baseRef.String
	rr.Reason = reason.String
	rr.FailureClass = failureClass.String
	rr.ParentRunID = parentID
	rr.SupersededByID = supersededBy
	if installID.Valid {
		rr.InstallationID = installID.Int64
	}
	if t, err := time.Parse(time.RFC3339Nano, startedStr); err == nil {
		rr.StartedAt = t
	}
	if endedPtr.Valid {
		if t, err := time.Parse(time.RFC3339Nano, endedPtr.String); err == nil {
			rr.EndedAt = &t
		}
	}
	if durPtr.Valid {
		v := durPtr.Int64
		rr.DurationMS = &v
	}
	rr.Error = errPtr.String
	if heartbeatPtr.Valid {
		if t, err := time.Parse(time.RFC3339Nano, heartbeatPtr.String); err == nil {
			rr.LastHeartbeatAt = &t
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, createdStr); err == nil {
		rr.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, updatedStr); err == nil {
		rr.UpdatedAt = t
	}
	return rr, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullTimePtr(p *time.Time) any {
	if p == nil {
		return nil
	}
	return p.UTC().Format(time.RFC3339Nano)
}
