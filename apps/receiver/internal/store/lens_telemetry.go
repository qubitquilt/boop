package store

// Per-lens telemetry (QUB-109).
//
// The aggregate telemetry table is one row per run — the
// dashboard's "cost per run" view reads it. The dashboard's
// "cost per lens" view (Phase 4's "Costs & lenses" page)
// needs a row per (run, lens) so the lens rollup is a simple
// GROUP BY lens. The runner parses `lens: <name>` markers
// from the orchestrator's output (decoupled from prompt
// layout — the meta-review refactor in QUB-96 won't break
// attribution) and POSTs a batch at end-of-run.
//
// The model/provider fields mirror the aggregate telemetry
// so the dashboard can render "$/lens" leaderboards and
// "$/(lens × model)" matrices from the same shape. Cost is
// this lens's contribution; the aggregate row carries the
// total so the math reconciles: sum(lens_telemetry.cost_usd)
// == telemetry.cost_usd.

import (
	"context"
	"fmt"
	"time"
)

// LensTelemetry is one lens's contribution to a run's
// aggregate telemetry. RunID+Lens is the natural key; the
// runner's batch replace drops + re-inserts on each delivery
// so the dashboard never sees a half-applied batch.
// LensTelemetry is one lens's contribution to a run's
// aggregate telemetry. RunID+Lens is the natural key; the
// runner's batch replace drops + re-inserts on each delivery
// so the dashboard never sees a half-applied batch.
// JSON tags for the /api/runs/{id}/lens_telemetry surface.
type LensTelemetry struct {
	ID               int64     `json:"id"`
	RunID            string    `json:"run_id"`
	Lens             string    `json:"lens"`
	Model            string    `json:"model,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	StepCount        int       `json:"step_count"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// ReplaceLensTelemetry atomically replaces the per-lens
// rows for the given run. The transaction wraps the
// DELETE + INSERTs so a partial batch never lands; either
// the dashboard sees the old rows or the new ones, never
// a half-applied mix.
//
// An empty batch is allowed and is treated as a "this run
// had no per-lens attribution" — the table is left empty
// for the run. The dashboard renders a "-" for the lens
// breakdown in that case rather than a stale row.
func (s *Store) ReplaceLensTelemetry(ctx context.Context, runID string, rows []LensTelemetry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin lens tel tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM lens_telemetry WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("store: clear lens tel: %w", err)
	}
	if len(rows) == 0 {
		return tx.Commit()
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO lens_telemetry (
			run_id, lens, model, provider,
			input_tokens, output_tokens, reasoning_tokens,
			cache_read_tokens, cache_write_tokens,
			cost_usd, step_count, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("store: prepare lens tel: %w", err)
	}
	defer stmt.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.RunID, r.Lens, nullString(r.Model), nullString(r.Provider),
			r.InputTokens, r.OutputTokens, r.ReasoningTokens,
			r.CacheReadTokens, r.CacheWriteTokens,
			r.CostUSD, r.StepCount, now,
		); err != nil {
			return fmt.Errorf("store: insert lens tel %s/%s: %w", r.RunID, r.Lens, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit lens tel: %w", err)
	}
	return nil
}

// ListLensTelemetry returns the per-lens rows for a run,
// ordered by cost descending so the dashboard's run-detail
// page can show the dominant lens first.
func (s *Store) ListLensTelemetry(ctx context.Context, runID string) ([]LensTelemetry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, lens,
			COALESCE(model, ''), COALESCE(provider, ''),
			COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
			COALESCE(reasoning_tokens, 0), COALESCE(cache_read_tokens, 0),
			COALESCE(cache_write_tokens, 0),
			COALESCE(cost_usd, 0), COALESCE(step_count, 0),
			recorded_at
		FROM lens_telemetry WHERE run_id = ?
		ORDER BY cost_usd DESC, lens ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list lens tel: %w", err)
	}
	defer rows.Close()
	var out []LensTelemetry
	for rows.Next() {
		var (
			lt    LensTelemetry
			recAt string
		)
		if err := rows.Scan(
			&lt.ID, &lt.RunID, &lt.Lens,
			&lt.Model, &lt.Provider,
			&lt.InputTokens, &lt.OutputTokens,
			&lt.ReasoningTokens, &lt.CacheReadTokens,
			&lt.CacheWriteTokens,
			&lt.CostUSD, &lt.StepCount,
			&recAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan lens tel: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, recAt); err == nil {
			lt.RecordedAt = t
		}
		out = append(out, lt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lens tel rows: %w", err)
	}
	return out, nil
}

// LensCostRollup is the dashboard's "cost by lens" view
// (Phase 4's "Costs & lenses" page). One row per lens
// with the aggregate cost / tokens / step_count across
// the time window. Model breakdown is a separate query;
// this is the leaderboard.
type LensCostRollup struct {
	Lens            string  `json:"lens"`
	RunCount        int     `json:"run_count"`
	CostUSD         float64 `json:"cost_usd"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	StepCount       int     `json:"step_count"`
}

// LensCostSummary returns the cost-by-lens rollup over the
// given time window. Phase 4 renders this as the
// "$/lens leaderboard" — the load-bearing reason for
// the lens_telemetry table. The "lens is the row grain"
// rule in the spec is the reason this query exists as a
// separate endpoint; rolling cost up to $/PR or $/day
// would hide the lens that is actually expensive (deep
// dominates).
func (s *Store) LensCostSummary(ctx context.Context, from, to time.Time) ([]LensCostRollup, error) {
	q := `
		SELECT
			lt.lens,
			COUNT(DISTINCT lt.run_id) AS run_count,
			COALESCE(SUM(lt.cost_usd), 0) AS cost_usd,
			COALESCE(SUM(lt.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(lt.output_tokens), 0) AS output_tokens,
			COALESCE(SUM(lt.reasoning_tokens), 0) AS reasoning_tokens,
			COALESCE(SUM(lt.step_count), 0) AS step_count
		FROM lens_telemetry lt
		JOIN runs r ON r.id = lt.run_id
		WHERE r.started_at >= ? AND r.started_at <= ?
		GROUP BY lt.lens
		ORDER BY cost_usd DESC
	`
	rows, err := s.db.QueryContext(ctx, q,
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("store: lens cost summary: %w", err)
	}
	defer rows.Close()
	var out []LensCostRollup
	for rows.Next() {
		var r LensCostRollup
		if err := rows.Scan(&r.Lens, &r.RunCount, &r.CostUSD,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.StepCount); err != nil {
			return nil, fmt.Errorf("store: scan lens cost: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lens cost rows: %w", err)
	}
	return out, nil
}
