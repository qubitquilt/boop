package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StatsBucket is the granularity at which the dashboard rolls up
// runs over time. Day is the default (a 30-day window fits the
// dashboard's overview); hour and week are exposed for the
// "live" and "trends" pages.
type StatsBucket string

const (
	BucketHour StatsBucket = "hour"
	BucketDay  StatsBucket = "day"
	BucketWeek StatsBucket = "week"
)

// SummaryStats are the top-line KPIs the dashboard shows in the
// overview header. Counts and ratios are computed in SQL; the
// "current streak" fields are computed in Go because the streak
// is a function of the run's chronological order, not a single
// aggregate. JSON tags added for stable /api/stats wire format.
type SummaryStats struct {
	TotalRuns      int64   `json:"total_runs"`
	SucceededRuns  int64   `json:"succeeded_runs"`
	FailedRuns     int64   `json:"failed_runs"`
	RunningRuns    int64   `json:"running_runs"`
	SuccessRate    float64 `json:"success_rate"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	TotalTokens    int64   `json:"total_tokens"`
	AvgDurationMS  int64   `json:"avg_duration_ms"`
	P50DurationMS  int64   `json:"p50_duration_ms"`
	P95DurationMS  int64   `json:"p95_duration_ms"`
	UniqueRepos    int64   `json:"unique_repos"`
	UniqueInstalls int64   `json:"unique_installs"`
}

// BucketPoint is one bar on the time-series chart.
type BucketPoint struct {
	BucketStart  time.Time `json:"bucket_start"`
	Runs         int64     `json:"runs"`
	Succeeded    int64     `json:"succeeded"`
	Failed       int64     `json:"failed"`
	CostUSD      float64   `json:"cost_usd"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// GroupBy selects the dimension for per-bucket aggregation. The
// default ("") collapses everything into the time bucket.
type GroupBy string

const (
	GroupByRepo  GroupBy = "repo"
	GroupByModel GroupBy = "model"
)

// GroupBucketPoint is a BucketPoint tagged with the group key.
type GroupBucketPoint struct {
	BucketStart time.Time
	Key         string
	Runs        int64
	CostUSD     float64
	Tokens      int64
}

// Summary returns the top-line KPIs for [from, to]. durationMS
// stats (avg/p50/p95) are computed from completed runs only —
// pending or running runs have no meaningful duration and would
// skew the percentiles toward zero.
func (s *Store) Summary(ctx context.Context, from, to time.Time) (SummaryStats, error) {
	var out SummaryStats
	// avg is computed against succeeded-only to match the
	// dashboard's "average review time" KPI; COALESCE on the
	// divisor keeps an empty window from raising an error.
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'running'   THEN 1 ELSE 0 END), 0),
			COALESCE(CAST(SUM(CASE WHEN status = 'succeeded' AND duration_ms IS NOT NULL THEN duration_ms ELSE 0 END) AS REAL) /
			         NULLIF(SUM(CASE WHEN status = 'succeeded' AND duration_ms IS NOT NULL THEN 1 ELSE 0 END), 0), 0)
		FROM runs
		WHERE started_at >= ? AND started_at <= ?
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)).Scan(
		&out.TotalRuns, &out.SucceededRuns, &out.FailedRuns, &out.RunningRuns, &out.AvgDurationMS,
	)
	if err != nil {
		return SummaryStats{}, fmt.Errorf("store: summary counts: %w", err)
	}
	if out.TotalRuns > 0 {
		out.SuccessRate = float64(out.SucceededRuns) / float64(out.TotalRuns)
	}

	// Percentiles via a subquery — SQLite has no native
	// percentile_disc, but the standard trick is a self-join
	// with row_number. We pull only the completed durations
	// into a sorted slice in Go; for a 30-day window that's at
	// most a few thousand rows, well within the dashboard's
	// load profile.
	rows, err := s.db.QueryContext(ctx, `
		SELECT duration_ms FROM runs
		WHERE started_at >= ? AND started_at <= ?
		  AND status = 'succeeded' AND duration_ms IS NOT NULL
		ORDER BY duration_ms
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return SummaryStats{}, fmt.Errorf("store: summary durations: %w", err)
	}
	defer rows.Close()
	var durations []int64
	for rows.Next() {
		var d sql.NullInt64
		if err := rows.Scan(&d); err != nil {
			return SummaryStats{}, fmt.Errorf("store: summary duration scan: %w", err)
		}
		if d.Valid {
			durations = append(durations, d.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return SummaryStats{}, fmt.Errorf("store: summary duration rows: %w", err)
	}
	if n := len(durations); n > 0 {
		out.P50DurationMS = durations[clampIdx(n, 0.50)]
		out.P95DurationMS = durations[clampIdx(n, 0.95)]
	}

	// Cost + tokens from the joined telemetry table. Sum of
	// total tokens = input + output + cache_read + cache_write +
	// reasoning. The breakdown is on the drill-down; the
	// top-line shows the all-in count.
	cost, tokens, err := s.sumCostAndTokens(ctx, from, to)
	if err != nil {
		return SummaryStats{}, err
	}
	out.TotalCostUSD = cost
	out.TotalTokens = tokens

	// Unique repos and installations: cheap DISTINCT counts
	// over the runs table. The dashboard uses these to say
	// "X repos reviewed this month".
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT owner || '/' || repo) FROM runs
		WHERE started_at >= ? AND started_at <= ?
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)).Scan(&out.UniqueRepos); err != nil {
		return SummaryStats{}, fmt.Errorf("store: unique repos: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT installation_id) FROM runs
		WHERE started_at >= ? AND started_at <= ?
		  AND installation_id IS NOT NULL
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)).Scan(&out.UniqueInstalls); err != nil {
		return SummaryStats{}, fmt.Errorf("store: unique installs: %w", err)
	}

	return out, nil
}

func (s *Store) sumCostAndTokens(ctx context.Context, from, to time.Time) (float64, int64, error) {
	var (
		cost   sql.NullFloat64
		tokens sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(t.cost_usd), 0),
			COALESCE(SUM(t.input_tokens + t.output_tokens + t.reasoning_tokens + t.cache_read_tokens + t.cache_write_tokens), 0)
		FROM telemetry t
		JOIN runs r ON r.id = t.run_id
		WHERE r.started_at >= ? AND r.started_at <= ?
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)).Scan(&cost, &tokens)
	if err != nil {
		return 0, 0, fmt.Errorf("store: cost+tokens: %w", err)
	}
	return cost.Float64, tokens.Int64, nil
}

// BucketSeries returns one BucketPoint per time bucket in [from,
// to]. The bucket column uses strftime so we can swap hour/day/week
// via the bucket argument. UTC throughout — the dashboard renders
// in the operator's local TZ, but the stored data is always UTC to
// keep DST from double-counting the boundary hours.
func (s *Store) BucketSeries(ctx context.Context, from, to time.Time, bucket StatsBucket) ([]BucketPoint, error) {
	if bucket == "" {
		bucket = BucketDay
	}
	format, err := bucketFormat(bucket)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			strftime(?, r.started_at) AS bucket,
			COUNT(r.id),
			COALESCE(SUM(CASE WHEN r.status = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.status = 'failed'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(t.cost_usd), 0),
			COALESCE(SUM(t.input_tokens), 0),
			COALESCE(SUM(t.output_tokens), 0)
		FROM runs r
		LEFT JOIN telemetry t ON t.run_id = r.id
		WHERE r.started_at >= ? AND r.started_at <= ?
		GROUP BY bucket
		ORDER BY bucket
	`, format, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("store: bucket series: %w", err)
	}
	defer rows.Close()
	var out []BucketPoint
	for rows.Next() {
		var (
			bucketStr string
			bp        BucketPoint
		)
		if err := rows.Scan(&bucketStr, &bp.Runs, &bp.Succeeded, &bp.Failed, &bp.CostUSD, &bp.InputTokens, &bp.OutputTokens); err != nil {
			return nil, fmt.Errorf("store: bucket scan: %w", err)
		}
		t, err := time.Parse(bucketFormatForParse(bucket), bucketStr)
		if err != nil {
			return nil, fmt.Errorf("store: bucket parse %q: %w", bucketStr, err)
		}
		bp.BucketStart = t
		out = append(out, bp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: bucket rows: %w", err)
	}
	return out, nil
}

// GroupedBucketSeries returns one row per (bucket, key) pair.
// keyExpr is a SQL expression selecting the group column; e.g.
// "(owner || '/' || repo)" or "t.model". Kept as a string for
// flexibility, but the call sites are typed via the GroupBy enum
// so the only two values that ever reach the SQL are vetted.
func (s *Store) GroupedBucketSeries(ctx context.Context, from, to time.Time, bucket StatsBucket, group GroupBy) ([]GroupBucketPoint, error) {
	if bucket == "" {
		bucket = BucketDay
	}
	format, err := bucketFormat(bucket)
	if err != nil {
		return nil, err
	}
	keyExpr, err := groupKeyExpr(group)
	if err != nil {
		return nil, err
	}

	q := fmt.Sprintf(`
		SELECT
			strftime(?, r.started_at) AS bucket,
			COALESCE(%s, '(unknown)') AS key,
			COUNT(r.id),
			COALESCE(SUM(t.cost_usd), 0),
			COALESCE(SUM(t.input_tokens + t.output_tokens + t.reasoning_tokens + t.cache_read_tokens + t.cache_write_tokens), 0)
		FROM runs r
		LEFT JOIN telemetry t ON t.run_id = r.id
		WHERE r.started_at >= ? AND r.started_at <= ?
		GROUP BY bucket, key
		ORDER BY bucket, key
	`, keyExpr)

	rows, err := s.db.QueryContext(ctx, q,
		format,
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("store: grouped series: %w", err)
	}
	defer rows.Close()
	var out []GroupBucketPoint
	for rows.Next() {
		var (
			bucketStr string
			gp        GroupBucketPoint
		)
		if err := rows.Scan(&bucketStr, &gp.Key, &gp.Runs, &gp.CostUSD, &gp.Tokens); err != nil {
			return nil, fmt.Errorf("store: grouped scan: %w", err)
		}
		t, err := time.Parse(bucketFormatForParse(bucket), bucketStr)
		if err != nil {
			return nil, fmt.Errorf("store: grouped parse %q: %w", bucketStr, err)
		}
		gp.BucketStart = t
		out = append(out, gp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: grouped rows: %w", err)
	}
	return out, nil
}

// RepoRollup is one row of the per-repo leaderboard on the
// dashboard's Repositories page. Limited to repos that have at
// least one completed run in the window; an "all-time" call with
// a wide window is the right shape for the side panel.
type RepoRollup struct {
	Owner        string    `json:"owner"`
	Repo         string    `json:"repo"`
	Runs         int64     `json:"runs"`
	Succeeded    int64     `json:"succeeded"`
	Failed       int64     `json:"failed"`
	SuccessRate  float64   `json:"success_rate"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	LastRunAt    time.Time `json:"last_run_at"`
}

// PerRepo returns one RepoRollup per repo, ordered by total runs
// desc, then by total cost desc. Capped to 200 repos; the
// dashboard paginates beyond that.
func (s *Store) PerRepo(ctx context.Context, from, to time.Time, limit int) ([]RepoRollup, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			r.owner,
			r.repo,
			COUNT(r.id),
			COALESCE(SUM(CASE WHEN r.status = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.status = 'failed'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(t.cost_usd), 0),
			MAX(r.started_at)
		FROM runs r
		LEFT JOIN telemetry t ON t.run_id = r.id
		WHERE r.started_at >= ? AND r.started_at <= ?
		GROUP BY r.owner, r.repo
		ORDER BY COUNT(r.id) DESC, COALESCE(SUM(t.cost_usd), 0) DESC
		LIMIT ?
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("store: per repo: %w", err)
	}
	defer rows.Close()
	var out []RepoRollup
	for rows.Next() {
		var (
			rr     RepoRollup
			lastAt string
		)
		if err := rows.Scan(&rr.Owner, &rr.Repo, &rr.Runs, &rr.Succeeded, &rr.Failed, &rr.TotalCostUSD, &lastAt); err != nil {
			return nil, fmt.Errorf("store: per repo scan: %w", err)
		}
		if rr.Runs > 0 {
			rr.SuccessRate = float64(rr.Succeeded) / float64(rr.Runs)
		}
		if t, err := time.Parse(time.RFC3339Nano, lastAt); err == nil {
			rr.LastRunAt = t
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: per repo rows: %w", err)
	}
	return out, nil
}

// ModelRollup is the per-model breakdown. Same shape as
// RepoRollup; the dashboard renders these as a stacked-bar chart
// on the cost page.
type ModelRollup struct {
	Model        string  `json:"model"`
	Runs         int64   `json:"runs"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// PerModel returns the per-model rollup. Pulled from the
// telemetry table directly; runs without telemetry are excluded
// (a run that didn't reach the SDK call has no model to
// attribute to).
func (s *Store) PerModel(ctx context.Context, from, to time.Time) ([]ModelRollup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.model,
			COUNT(t.run_id),
			COALESCE(SUM(t.cost_usd), 0),
			COALESCE(SUM(t.input_tokens), 0),
			COALESCE(SUM(t.output_tokens), 0)
		FROM telemetry t
		JOIN runs r ON r.id = t.run_id
		WHERE r.started_at >= ? AND r.started_at <= ?
		GROUP BY t.model
		ORDER BY SUM(t.cost_usd) DESC
	`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("store: per model: %w", err)
	}
	defer rows.Close()
	var out []ModelRollup
	for rows.Next() {
		var m ModelRollup
		if err := rows.Scan(&m.Model, &m.Runs, &m.TotalCostUSD, &m.InputTokens, &m.OutputTokens); err != nil {
			return nil, fmt.Errorf("store: per model scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: per model rows: %w", err)
	}
	return out, nil
}

// bucketFormat returns the strftime format string SQLite uses to
// bucket timestamps. The format is what we use to GROUP BY; the
// output of strftime matches strftimeFormatForParse so we can
// round-trip the result back to a time.Time.
//
// SQLite strftime format specifiers we rely on:
//
//	%Y  4-digit year
//	%m  2-digit month
//	%d  2-digit day
//	%H  2-digit hour (24h)
//	%W  week of year (00..53, Monday-based — close enough to ISO
//	     for the dashboard's purpose; full ISO week is not
//	     supported by SQLite and rolling our own is not worth
//	     the test surface).
func bucketFormat(b StatsBucket) (string, error) {
	switch b {
	case BucketHour:
		return "%Y-%m-%dT%H:00:00Z", nil
	case BucketDay:
		return "%Y-%m-%dT00:00:00Z", nil
	case BucketWeek:
		return "%Y-W%W", nil
	}
	return "", fmt.Errorf("store: unknown bucket %q", b)
}

func bucketFormatForParse(b StatsBucket) string {
	switch b {
	case BucketHour, BucketDay:
		return "2006-01-02T15:04:05Z"
	case BucketWeek:
		return "2006-W05"
	}
	return time.RFC3339
}

func groupKeyExpr(g GroupBy) (string, error) {
	switch g {
	case GroupByRepo:
		return "(r.owner || '/' || r.repo)", nil
	case GroupByModel:
		return "t.model", nil
	}
	return "", fmt.Errorf("store: unknown group %q", g)
}

// clampIdx returns the index of the p-th percentile (0..1) in a
// sorted slice of length n. Rounds half-up so the median of a
// 4-element slice lands on the upper middle, matching the
// common "linear interpolation" rule.
func clampIdx(n int, p float64) int {
	if n <= 0 {
		return 0
	}
	idx := int(float64(n) * p)
	if idx >= n {
		idx = n - 1
	}
	return idx
}
