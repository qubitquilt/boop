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
// Field names mirror the receiver's wire format (snake_case JSON
// tags) so the CLI and other API consumers get stable keys.
//
// QUB-105: every field the OpenRouter SDK exposes on ChatUsage
// now lands on a typed column. Nullable columns (request_id,
// duration_ms, error_status_code, error_content_type) use
// pointers so the JSON shape distinguishes "missing" from "zero".
// Default-zero columns use plain scalars; the SQL DEFAULT clauses
// fill missing values for a partial payload (pre-QUB-105 runner
// posting against a v6 schema still lands a usable row).
type Telemetry struct {
	RunID                 string    `json:"run_id"`
	Model                 string    `json:"model"`
	Provider              string    `json:"provider,omitempty"`
	InputTokens           int64     `json:"input_tokens"`
	OutputTokens          int64     `json:"output_tokens"`
	TotalTokens           int64     `json:"total_tokens"`
	ReasoningTokens       int64     `json:"reasoning_tokens"`
	CacheReadTokens       int64     `json:"cache_read_tokens"`
	CacheWriteTokens      int64     `json:"cache_write_tokens"`
	CostUSD               float64   `json:"cost_usd"`
	CostPromptUSD         float64   `json:"cost_prompt_usd"`
	CostCompletionUSD     float64   `json:"cost_completion_usd"`
	CostUpstreamUSD       float64   `json:"cost_upstream_usd"`
	IsByok                bool      `json:"is_byok"`
	ServerToolCallsExec   int64     `json:"server_tool_calls_executed"`
	ServerToolCallsReq    int64     `json:"server_tool_calls_requested"`
	RequestID             *string   `json:"request_id,omitempty"`
	DurationMS            *int64    `json:"duration_ms,omitempty"`
	StepCount             int       `json:"step_count"`
	RecordedAt            time.Time `json:"recorded_at"`
	Error                 *string   `json:"error,omitempty"`
	ErrorStatusCode       *int64    `json:"error_status_code,omitempty"`
	ErrorContentType      *string   `json:"error_content_type,omitempty"`
	ErrorBody             *string   `json:"error_body,omitempty"`
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
			input_tokens, output_tokens, total_tokens,
			reasoning_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, cost_prompt_usd, cost_completion_usd, cost_upstream_usd,
			is_byok, server_tool_calls_executed, server_tool_calls_requested,
			request_id, duration_ms,
			step_count, recorded_at,
			error, error_status_code, error_content_type, error_body
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		t.RunID, t.Model, t.Provider,
		t.InputTokens, t.OutputTokens, t.TotalTokens,
		t.ReasoningTokens, t.CacheReadTokens, t.CacheWriteTokens,
		t.CostUSD, t.CostPromptUSD, t.CostCompletionUSD, t.CostUpstreamUSD,
		boolToInt(t.IsByok), t.ServerToolCallsExec, t.ServerToolCallsReq,
		nullableString(t.RequestID), nullableInt64(t.DurationMS),
		t.StepCount, t.RecordedAt.UTC().Format(time.RFC3339Nano),
		nullableString(t.Error), nullableInt64(t.ErrorStatusCode), nullableString(t.ErrorContentType), nullableString(t.ErrorBody),
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
			input_tokens, output_tokens, total_tokens,
			reasoning_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, cost_prompt_usd, cost_completion_usd, cost_upstream_usd,
			is_byok, server_tool_calls_executed, server_tool_calls_requested,
			request_id, duration_ms,
			step_count, recorded_at,
			error, error_status_code, error_content_type, error_body
		FROM telemetry WHERE run_id = ?
	`, runID)
	var (
		t            Telemetry
		recordedAt   string
		provider     sql.NullString
		requestID    sql.NullString
		durationMS   sql.NullInt64
		errStr       sql.NullString
		errStatus    sql.NullInt64
		errContentTp sql.NullString
		errBody      sql.NullString
		isByok       int
	)
	if err := row.Scan(
		&t.RunID, &t.Model, &provider,
		&t.InputTokens, &t.OutputTokens, &t.TotalTokens,
		&t.ReasoningTokens, &t.CacheReadTokens, &t.CacheWriteTokens,
		&t.CostUSD, &t.CostPromptUSD, &t.CostCompletionUSD, &t.CostUpstreamUSD,
		&isByok, &t.ServerToolCallsExec, &t.ServerToolCallsReq,
		&requestID, &durationMS,
		&t.StepCount, &recordedAt,
		&errStr, &errStatus, &errContentTp, &errBody,
	); err != nil {
		return Telemetry{}, err
	}
	t.Provider = provider.String
	t.IsByok = isByok != 0
	if requestID.Valid {
		v := requestID.String
		t.RequestID = &v
	}
	if durationMS.Valid {
		v := durationMS.Int64
		t.DurationMS = &v
	}
	if errStr.Valid {
		v := errStr.String
		t.Error = &v
	}
	if errStatus.Valid {
		v := errStatus.Int64
		t.ErrorStatusCode = &v
	}
	if errContentTp.Valid {
		v := errContentTp.String
		t.ErrorContentType = &v
	}
	if errBody.Valid {
		v := errBody.String
		t.ErrorBody = &v
	}
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

// boolToInt is defined in installations.go (used by both
// Installations.UpsertInstallations and RecordTelemetry).

// nullableString turns a *string into an interface that the
// SQLite driver writes as NULL when the pointer is nil. The
// telemetry table has nullable TEXT columns for request_id and
// error_content_type; using a Go pointer keeps the "absent vs
// empty string" distinction visible in the JSON wire shape too.
func nullableString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

// nullableInt64 mirrors nullableString for *int64 (the
// telemetry table's duration_ms and error_status_code columns).
func nullableInt64(n *int64) interface{} {
	if n == nil {
		return nil
	}
	return *n
}
