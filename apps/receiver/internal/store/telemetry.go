package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrUnknownRun is the sentinel returned by RecordTelemetry when
// the parent run row does not exist AND the store's
// "placeholder on miss" behavior is unavailable (see
// Config.InsertPlaceholderOnUnknownRun in the store options). The
// webhook handler matches this error with errors.Is and responds
// 202 to the runner so a transient race between the runner and
// the receiver's UpsertRun does not turn into a hard failure.
//
// In the default mode (placeholder enabled), RecordTelemetry
// never returns this — it INSERT OR IGNORE's a placeholder run
// row first, so the FK check passes and the telemetry is written.
// The sentinel exists for the strict path and for tests that want
// to assert the unknown-run branch.
var ErrUnknownRun = errors.New("store: unknown run")

// Telemetry is the LLM usage record for a single run. It is the
// source of truth for cost reporting and per-model breakdowns;
// without it the dashboard can only show "a review happened", not
// what the review cost.
//
// Field names mirror the OpenCode step_finish event shape so the
// JSON the runner POSTs maps 1:1 onto the struct. The runner is
// the only writer.
type Telemetry struct {
	RunID            string
	Model            string
	Provider         string
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	StepCount        int
	RecordedAt       time.Time
}

// RecordTelemetry inserts or replaces the telemetry row for a run.
// We REPLACE rather than UPSERT because there is exactly one
// telemetry record per run (the runner posts it once at the end of
// the review). A re-delivery from the runner is a no-op — the row
// is rewritten with the same shape.
//
// QUB-101: the runner's POST can land before the receiver's
// UpsertRun has committed (the runner is a separate process and
// the only sync point is the K8s Job existing). With the new
// submitJob ordering, UpsertRun runs before createJob returns,
// so in normal flow the parent row is always present by the
// time the runner can POST. The unknown-run path is the safety
// net for an UpsertRun that never landed (DB write error,
// receiver crashed mid-flow) and returns ErrUnknownRun; the
// webhook handler matches the sentinel and responds 202 to
// the runner, so the cost data is dropped silently on the
// floor rather than turning into a hard failure. The runner
// does not retry telemetry, so the data is genuinely lost in
// this edge case; the alternative (INSERT OR IGNORE a
// placeholder run row) was rejected because UpsertRun's
// ON CONFLICT only updates mutable fields, leaving the
// placeholder's empty owner/repo in place forever and
// surfacing as orphan rows on the dashboard.
func (s *Store) RecordTelemetry(ctx context.Context, t Telemetry) error {
	if t.RunID == "" {
		return errors.New("store: RecordTelemetry: empty run id")
	}
	if t.Model == "" {
		return errors.New("store: RecordTelemetry: empty model")
	}
	if t.RecordedAt.IsZero() {
		t.RecordedAt = time.Now().UTC()
	}

	// FK check first; SQLite needs the parent row to exist
	// before the child can reference it. The check is one
	// indexed point-lookup; it is the common case once the
	// receiver's UpsertRun has landed.
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, t.RunID).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: telemetry for run %q: %w", t.RunID, ErrUnknownRun)
		}
		return fmt.Errorf("store: telemetry parent check: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO telemetry (
			run_id, model, provider,
			input_tokens, output_tokens, reasoning_tokens,
			cache_read_tokens, cache_write_tokens,
			cost_usd, step_count, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		t.RunID, t.Model, t.Provider,
		t.InputTokens, t.OutputTokens, t.ReasoningTokens,
		t.CacheReadTokens, t.CacheWriteTokens,
		t.CostUSD, t.StepCount, t.RecordedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store: insert telemetry: %w", err)
	}
	return nil
}

// GetTelemetry returns the telemetry for a run, or sql.ErrNoRows
// if none. Used by the dashboard's drill-down to surface token
// counts and cost on the run detail panel.
func (s *Store) GetTelemetry(ctx context.Context, runID string) (Telemetry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT run_id, model, provider,
			input_tokens, output_tokens, reasoning_tokens,
			cache_read_tokens, cache_write_tokens,
			cost_usd, step_count, recorded_at
		FROM telemetry WHERE run_id = ?
	`, runID)
	var (
		t          Telemetry
		recordedAt string
		provider   sql.NullString
	)
	if err := row.Scan(
		&t.RunID, &t.Model, &provider,
		&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens,
		&t.CacheReadTokens, &t.CacheWriteTokens,
		&t.CostUSD, &t.StepCount, &recordedAt,
	); err != nil {
		return Telemetry{}, err
	}
	t.Provider = provider.String
	if at, err := time.Parse(time.RFC3339Nano, recordedAt); err == nil {
		t.RecordedAt = at
	}
	return t, nil
}

// TotalCost returns the sum of cost_usd across runs in the given
// time window. Used by the dashboard's top-line "this month so
// far" KPI and by the projected-monthly rollup.
func (s *Store) TotalCost(ctx context.Context, from, to time.Time) (float64, error) {
	var v sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(t.cost_usd)
		FROM telemetry t
		JOIN runs r ON r.id = t.run_id
		WHERE r.started_at >= ? AND r.started_at <= ?
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("store: total cost: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Float64, nil
}
