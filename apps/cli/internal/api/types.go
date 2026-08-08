// Package api holds the request/response types that mirror the
// receiver's HTTP surface. The CLI does not import the receiver
// module (they are separate Go modules); instead it re-declares the
// JSON shapes here. The structs are intentionally stable and
// additive-only so a drift in the receiver's wire shape is visible as
// a JSON parse error rather than a silent field drop.
package api

import "time"

// Health is the CLI's synthesized representation of GET /health.
// The receiver returns the plain-text body "ok" (not JSON), so the
// CLI builds this struct with Status set to "ok" on any 2xx.
type Health struct {
	Status string `json:"status"`
}

// Review is one row in the /api/reviews response. The receiver's
// reviews.go Review struct is the source of truth; the CLI mirrors it
// via json tags so the names stay aligned.
type Review struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Owner          string `json:"owner,omitempty"`
	Repo           string `json:"repo,omitempty"`
	PR             int    `json:"pr,omitempty"`
	Commit         string `json:"commit,omitempty"`
	BaseRef        string `json:"baseRef,omitempty"`
	StartTime      string `json:"startTime,omitempty"`
	CompletionTime string `json:"completionTime,omitempty"`
	Duration       string `json:"duration,omitempty"`
	Status         string `json:"status"`
	Active         int32  `json:"active"`
	Succeeded      int32  `json:"succeeded"`
	Failed         int32  `json:"failed"`
}

// ReviewsResponse is the body of GET /api/reviews.
type ReviewsResponse struct {
	Active []Review `json:"active"`
	Recent []Review `json:"recent"`
	Failed []Review `json:"failed"`
}

// Installation is one row in the /api/installations response. Mirrors
// store.Installation.
type Installation struct {
	ID                  int64     `json:"id"`
	AccountLogin        string    `json:"account_login"`
	AccountType         string    `json:"account_type"`
	RepositorySelection string    `json:"repository_selection,omitempty"`
	InstalledAt         time.Time `json:"installed_at,omitempty"`
	FetchedAt           time.Time `json:"fetched_at"`
	Paused              bool      `json:"paused"`
	LensOptOut          []string  `json:"lens_opt_out,omitempty"`
}

// InstallationsResponse is the body of GET /api/installations.
type InstallationsResponse struct {
	Installations []Installation `json:"installations"`
	FetchedAt     time.Time      `json:"fetched_at"`
}

// RunStatus is the lifecycle state of a single review. Mirrors
// store.RunStatus.
type RunStatus string

const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
)

// Telemetry is the LLM usage record for a single run.
type Telemetry struct {
	RunID            string    `json:"run_id"`
	Model            string    `json:"model"`
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

// LensTelemetry is one lens's contribution to a run's aggregate
// telemetry.
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

// RunStage is one row in the run_stages table.
type RunStage struct {
	ID         int64      `json:"id"`
	RunID      string     `json:"run_id"`
	Stage      string     `json:"stage"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMS *int64     `json:"duration_ms,omitempty"`
	Meta       string     `json:"meta,omitempty"`
}

// Run is one review row. Mirrors store.Run.
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

// RunWithTelemetry pairs a Run with its Telemetry, if any. Mirrors
// the receiver's dashboard.go RunWithTelemetry.
type RunWithTelemetry struct {
	Run                 // anonymous embed; JSON fields promoted to top level
	Telemetry Telemetry `json:"telemetry,omitempty"`
}

// ListRunsResponse is the body of GET /api/runs.
type ListRunsResponse struct {
	Runs       []RunWithTelemetry `json:"runs"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// StatsBucket is the granularity of the time-series.
type StatsBucket string

const (
	BucketHour StatsBucket = "hour"
	BucketDay  StatsBucket = "day"
	BucketWeek StatsBucket = "week"
)

// SummaryStats are the top-line KPIs.
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

// RepoRollup is one row of the per-repo leaderboard.
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

// ModelRollup is the per-model breakdown.
type ModelRollup struct {
	Model        string  `json:"model"`
	Runs         int64   `json:"runs"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// StatsResponse is the body of GET /api/stats.
type StatsResponse struct {
	From    time.Time     `json:"from"`
	To      time.Time     `json:"to"`
	Bucket  StatsBucket   `json:"bucket"`
	Summary SummaryStats  `json:"summary"`
	Buckets []BucketPoint `json:"buckets"`
	ByRepo  []RepoRollup  `json:"by_repo"`
	ByModel []ModelRollup `json:"by_model"`
}

// RerunPreviewRun is the per-run slice of the rerun preview.
type RerunPreviewRun struct {
	RunID     string `json:"run_id"`
	JobName   string `json:"job_name"`
	Status    string `json:"status"`
	Model     string `json:"model,omitempty"`
	HeadSHA   string `json:"head_sha"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Duration  int64  `json:"duration_ms,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// RerunPreviewResponse is the body of GET /api/runs/{id}/rerun-preview.
type RerunPreviewResponse struct {
	Prior RerunPreviewRun `json:"prior"`
	New   RerunPreviewRun `json:"new"`
}

// RerunRequest is the body of POST /api/runs/{id}/rerun.
type RerunRequest struct {
	Confirm bool   `json:"confirm"`
	Reason  string `json:"reason"`
}

// RerunResponse is the body of POST /api/runs/{id}/rerun.
type RerunResponse struct {
	NewRunID    string `json:"new_run_id"`
	PriorRunID  string `json:"prior_run_id"`
	ParentRunID string `json:"parent_run_id"`
}
