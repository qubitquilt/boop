package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RunStage is one row in the run_stages table — a single named
// phase of a run's lifecycle (hmac_verify, dedup, job_submit,
// pod_schedule, image_pull, clone, <lens>, summary_post,
// comment_post, etc.). The runner POSTs to the receiver at
// each transition; the receiver stamps the row with the
// server's clock (Phase 2's load-bearing correctness rule:
// every stage timestamp is on one clock, never the runner's).
//
// The Meta blob is the per-stage escape hatch: comment_post
// stores {"path": "...", "line": N}, lens stages store
// {"model": "...", "tokens": N}. Indexed queries (the
// waterfall) never read Meta; the dashboard does.
type RunStage struct {
	ID         int64
	RunID      string
	Stage      string
	StartedAt  time.Time
	EndedAt    *time.Time
	DurationMS *int64
	Meta       string
}

// UpsertRunStage records the start or end of a stage. The
// caller passes the stage name, a server-stamped start time,
// and (optionally) a server-stamped end time. Re-deliveries
// for the same (run_id, stage) are idempotent — the UNIQUE
// constraint is what makes that safe.
//
// We do not split this into "start" and "end" methods
// because the runner is at-least-once: a re-delivery of the
// "start" POST after the "end" POST landed would either
// re-open a closed stage or be a no-op, depending on which
// way we split. Keeping it as one ON CONFLICT method means
// the runner can fire-and-forget: a re-delivery overwrites
// the start time but preserves the end time and duration.
func (s *Store) UpsertRunStage(ctx context.Context, st RunStage) (RunStage, error) {
	if st.RunID == "" {
		return RunStage{}, fmt.Errorf("store: UpsertRunStage: empty run_id")
	}
	if st.Stage == "" {
		return RunStage{}, fmt.Errorf("store: UpsertRunStage: empty stage")
	}
	now := time.Now().UTC()
	if st.StartedAt.IsZero() {
		st.StartedAt = now
	}
	startedStr := st.StartedAt.UTC().Format(time.RFC3339Nano)
	endedStr := nullTimePtr(st.EndedAt)
	dur := nullInt64Ptr(st.DurationMS)
	meta := nullString(st.Meta)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_stages (run_id, stage, started_at, ended_at, duration_ms, meta)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, stage) DO UPDATE SET
			ended_at    = COALESCE(excluded.ended_at, run_stages.ended_at),
			duration_ms = COALESCE(excluded.duration_ms, run_stages.duration_ms),
			meta        = COALESCE(excluded.meta, run_stages.meta)
	`,
		st.RunID, st.Stage, startedStr, endedStr, dur, meta,
	)
	if err != nil {
		return RunStage{}, fmt.Errorf("store: upsert run stage: %w", err)
	}
	return s.GetRunStage(ctx, st.RunID, st.Stage)
}

// GetRunStage returns the stage row for the (run, stage) pair,
// or sql.ErrNoRows if the runner hasn't POSTed it yet. The
// dashboard's waterfall uses ListRunStages, not this.
func (s *Store) GetRunStage(ctx context.Context, runID, stage string) (RunStage, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, stage, started_at, ended_at, duration_ms, COALESCE(meta, '')
		FROM run_stages WHERE run_id = ? AND stage = ?
	`, runID, stage)
	return scanRunStage(row)
}

// ListRunStages returns every stage row for a run, ordered
// by start time. The dashboard's waterfall renders this as a
// Gantt — duration_ms is the bar length, the start time is
// the bar's left edge.
func (s *Store) ListRunStages(ctx context.Context, runID string) ([]RunStage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, stage, started_at, ended_at, duration_ms, COALESCE(meta, '')
		FROM run_stages WHERE run_id = ?
		ORDER BY started_at ASC, id ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list run stages: %w", err)
	}
	defer rows.Close()
	var out []RunStage
	for rows.Next() {
		st, err := scanRunStage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan run stage: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: run stages rows: %w", err)
	}
	return out, nil
}

// TouchRunHeartbeat updates the run's last_heartbeat_at to
// now (server clock). Called on every runner heartbeat POST
// (Phase 2). The stuck-runs panel reads the gap between
// last_heartbeat_at and now: a 2-minute gap with status
// running = "stuck"; heartbeats arriving while the stage
// never advances = "hung LLM call" (different operator
// response: re-queue vs. investigate model latency).
//
// Returns sql.ErrNoRows if the run is missing (the runner
// started before UpsertRun committed — same as
// UpdateRunStatus, the heartbeat loop retries on the next
// tick).
func (s *Store) TouchRunHeartbeat(ctx context.Context, runID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET last_heartbeat_at = ?, updated_at = ? WHERE id = ?
	`, now, now, runID)
	if err != nil {
		return fmt.Errorf("store: touch heartbeat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: touch heartbeat rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListStuckRuns returns runs that are status=running and
// have not heartbeat'd within the given threshold. Used by
// the dashboard's stuck-runs panel (Phase 4) and the
// receiver's optional stuck-run alerter. Limit clamps to
// 1..200, default 50.
func (s *Store) ListStuckRuns(ctx context.Context, olderThan time.Duration, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class, last_heartbeat_at,
			COALESCE(parent_run_id, ''), COALESCE(superseded_by_id, ''),
			created_at, updated_at
		FROM runs
		WHERE status = 'running'
		  AND (last_heartbeat_at IS NULL OR last_heartbeat_at < ?)
		ORDER BY started_at ASC
		LIMIT ?
	`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list stuck runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan stuck run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stuck runs rows: %w", err)
	}
	return out, nil
}

// Refund is one row of the refunds audit table (Phase 5's
// "zero out dashboard cost" action). Every write is a
// separate row so the operator can see the full history;
// there is no UPDATE path. Tokens is the total token count
// being zeroed (input + output + reasoning); Lens identifies
// which lens's telemetry was zeroed (empty string for an
// "all lenses" zero-out).
type Refund struct {
	ID         int64
	RunID      string
	Lens       string
	Tokens     int64
	RefundedAt time.Time
	RefundedBy string
}

// RecordRefund appends a row to the refunds table. The
// caller (Phase 4 dashboard endpoint) passes the
// actor's identity (the BOOP_DASHBOARD_TOKEN bearer or a
// future per-user identity) as RefundedBy. No update path
// by design: every zero-out is its own audit event.
func (s *Store) RecordRefund(ctx context.Context, r Refund) (Refund, error) {
	if r.RefundedBy == "" {
		return Refund{}, fmt.Errorf("store: RecordRefund: empty refunded_by")
	}
	if r.RefundedAt.IsZero() {
		r.RefundedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO refunds (run_id, lens, tokens, refunded_at, refunded_by)
		VALUES (?, ?, ?, ?, ?)
	`,
		nullString(r.RunID), nullString(r.Lens), r.Tokens,
		r.RefundedAt.UTC().Format(time.RFC3339Nano), r.RefundedBy,
	)
	if err != nil {
		return Refund{}, fmt.Errorf("store: record refund: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Refund{}, fmt.Errorf("store: refund id: %w", err)
	}
	r.ID = id
	return r, nil
}

// ListRefunds returns the refund history for a run, newest
// first. The dashboard's "audit trail" view renders this
// alongside the run detail.
func (s *Store) ListRefunds(ctx context.Context, runID string) ([]Refund, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(run_id, ''), COALESCE(lens, ''), COALESCE(tokens, 0),
			refunded_at, refunded_by
		FROM refunds WHERE run_id = ?
		ORDER BY refunded_at DESC, id DESC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list refunds: %w", err)
	}
	defer rows.Close()
	var out []Refund
	for rows.Next() {
		var (
			r       Refund
			runID   string
			lens    string
			tok     int64
			refAt   string
		)
		if err := rows.Scan(&r.ID, &runID, &lens, &tok, &refAt, &r.RefundedBy); err != nil {
			return nil, fmt.Errorf("store: scan refund: %w", err)
		}
		r.RunID = runID
		r.Lens = lens
		r.Tokens = tok
		if t, err := time.Parse(time.RFC3339Nano, refAt); err == nil {
			r.RefundedAt = t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: refunds rows: %w", err)
	}
	return out, nil
}

func scanRunStage(r rowScanner) (RunStage, error) {
	var (
		st         RunStage
		startedStr string
		endedPtr   sql.NullString
		durPtr     sql.NullInt64
		meta       sql.NullString
	)
	if err := r.Scan(&st.ID, &st.RunID, &st.Stage, &startedStr, &endedPtr, &durPtr, &meta); err != nil {
		return RunStage{}, err
	}
	if t, err := time.Parse(time.RFC3339Nano, startedStr); err == nil {
		st.StartedAt = t
	}
	if endedPtr.Valid {
		if t, err := time.Parse(time.RFC3339Nano, endedPtr.String); err == nil {
			st.EndedAt = &t
		}
	}
	if durPtr.Valid {
		v := durPtr.Int64
		st.DurationMS = &v
	}
	st.Meta = meta.String
	return st, nil
}
