// Package webhook: dashboard data-layer handlers.
//
// These handlers are read-only (GET) plus two runner-only POST
// endpoints (telemetry, status). They share the same Handler
// struct as the webhook handler because they share the same
// dependencies (logger, ghClient, store), but live in a separate
// file so the webhook lifecycle in handler.go stays linear.
//
// Auth model:
//   - GET endpoints are open within the cluster. The dashboard
//     service is internal; if/when the dashboard moves behind an
//     Ingress, the auth layer sits in front of the dashboard, not
//     in the receiver.
//   - POST endpoints (telemetry, status) require a shared secret
//     in the X-BOOP-Runner-Token header. The secret is set via
//     Config.RunnerToken and propagated to the runner as an env
//     var in the Job template. This is the only thing keeping an
//     unauthenticated caller from polluting the dashboard data.
package webhook

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"github.com/michaelruelas/boop-receiver/internal/store"
)

// InstallationsResponse is the body of GET /api/installations.
// The dashboard renders one row per installation; the count is
// the dashboard's "X repos installed Boop" KPI.
type InstallationsResponse struct {
	Installations []boopgithub.Installation `json:"installations"`
	FetchedAt     time.Time                 `json:"fetched_at"`
}

// ListInstallations handles GET /api/installations. Returns the
// cached list from the store (refreshed every 5 min by a
// background poller started in main). If the store is empty (e.g.
// right after receiver start, before the first poll completes),
// the handler does a synchronous refresh so the dashboard
// doesn't show an empty list for the first 5 minutes.
func (h *Handler) ListInstallations(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()

	rows, err := h.store.ListInstallations(ctx)
	if err != nil {
		h.logger.Warn("list installations", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	fetchedAt, _ := h.store.LatestInstallationFetch(ctx)
	if len(rows) == 0 && h.ghClient != nil {
		// Cold start: do a synchronous refresh so the dashboard
		// doesn't have to wait up to 5 min. Errors here are
		// non-fatal — the dashboard shows an empty list and
		// the next poll will populate it.
		if fresh, ferr := h.ghClient.ListInstallations(ctx); ferr == nil {
			installs := make([]store.Installation, len(fresh))
			for i, ins := range fresh {
				installs[i] = store.Installation{
					ID:                  ins.ID,
					AccountLogin:        ins.AccountLogin,
					AccountType:         ins.AccountType,
					RepositorySelection: ins.RepositorySelection,
					InstalledAt:         ins.InstalledAt,
				}
			}
			if err := h.store.UpsertInstallations(ctx, installs); err == nil {
				rows = installs
				fetchedAt = time.Now().UTC()
			} else {
				// EH-010: a failed upsert here used to
				// be silently swallowed. The dashboard
				// would then render an empty list and the
				// operator would have no idea why. A
				// warn-level log is enough — the next
				// background poll will retry — but the
				// operator now sees the failure in the
				// receiver's log stream and can correlate
				// it with their dashboard complaint.
				h.logger.Warn("cold start upsert installations", "err", err)
			}
		} else {
			h.logger.Warn("cold start refresh installations", "err", ferr)
		}
	}

	// The store's Installation and the GitHub manager's
	// Installation are deliberately separate types — neither
	// package should import the other. Convert at the wire
	// boundary so the dashboard gets a single shape.
	out := make([]boopgithub.Installation, len(rows))
	for i, ins := range rows {
		out[i] = boopgithub.Installation{
			ID:                  ins.ID,
			AccountLogin:        ins.AccountLogin,
			AccountType:         ins.AccountType,
			RepositorySelection: ins.RepositorySelection,
			InstalledAt:         ins.InstalledAt,
		}
	}
	writeJSON(w, http.StatusOK, InstallationsResponse{
		Installations: out,
		FetchedAt:     fetchedAt,
	})
}

// ListRuns handles GET /api/runs. All filters are optional;
// missing filters mean "no constraint". The query string keys
// match the dashboard's URL shape:
//
//	from=rfc3339    inclusive lower bound on started_at
//	to=rfc3339      inclusive upper bound on started_at
//	owner=...       exact match on owner
//	repo=...        exact match on repo
//	status=...      one of pending|running|succeeded|failed
//	installation=   exact match on installation_id
//	cursor=...      keyset cursor from a previous response
//	limit=N         page size, clamped to 1..200, default 50
//
// The response includes next_cursor when more rows exist. An
// empty next_cursor means "no more pages".
func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	f := store.ListRunsFilter{
		Owner:  q.Get("owner"),
		Repo:   q.Get("repo"),
		Cursor: q.Get("cursor"),
	}
	if s := q.Get("status"); s != "" {
		f.Status = store.RunStatus(s)
	}
	if s := q.Get("installation"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			f.InstallationID = n
		}
	}
	if s := q.Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.From = t
		}
	}
	if s := q.Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.To = t
		}
	}
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Limit = n
		}
	}

	// SP-006: bulk fetch (runs + telemetry in one batch)
	// replaces the N+1 GetTelemetry loop. The dashboard's
	// run-list wants every run with its cost column, so
	// the join is the load-bearing shape.
	page, err := h.store.ListRunsWithTelemetry(r.Context(), f)
	if err != nil {
		h.logger.Warn("list runs", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]RunWithTelemetry, 0, len(page.Runs))
	for _, rt := range page.Runs {
		out = append(out, RunWithTelemetry{Run: rt.Run, Telemetry: rt.Telemetry})
	}
	writeJSON(w, http.StatusOK, ListRunsResponse{
		Runs:       out,
		NextCursor: page.NextCursor,
	})
}

// RunWithTelemetry pairs a Run with its Telemetry, if any. The
// dashboard's runs table shows cost + tokens on each row, and a
// separate API call would mean a join on the client side. Cheap
// to assemble here.
type RunWithTelemetry struct {
	store.Run
	Telemetry store.Telemetry `json:"telemetry,omitempty"`
}

// ListRunsResponse is the body of GET /api/runs.
type ListRunsResponse struct {
	Runs       []RunWithTelemetry `json:"runs"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// GetRun handles GET /api/runs/{id}. Returns a single run with its
// telemetry, or 404 if the run does not exist.
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	run, err := h.store.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("get run", "id", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	var telem store.Telemetry
	if t, err := h.store.GetTelemetry(ctx, id); err == nil {
		telem = t
	}
	writeJSON(w, http.StatusOK, RunWithTelemetry{Run: run, Telemetry: telem})
}

// StatsResponse is the body of GET /api/stats. The shape is
// stable: summary is the top-line KPI block, buckets is the
// time-series, by_repo and by_model are the leaderboards. The
// dashboard renders this in one fetch.
type StatsResponse struct {
	From    time.Time           `json:"from"`
	To      time.Time           `json:"to"`
	Bucket  store.StatsBucket   `json:"bucket"`
	Summary store.SummaryStats  `json:"summary"`
	Buckets []store.BucketPoint `json:"buckets"`
	ByRepo  []store.RepoRollup  `json:"by_repo"`
	ByModel []store.ModelRollup `json:"by_model"`
}

// Stats handles GET /api/stats. Defaults: 30-day window ending
// now, day-bucketed series, top 50 repos / all models. The
// dashboard can override any of these via query string.
func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	from := now.Add(-30 * 24 * time.Hour)
	to := now
	if s := q.Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		}
	}
	if s := q.Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		}
	}
	if to.Before(from) {
		http.Error(w, "to < from", http.StatusBadRequest)
		return
	}
	bucket := store.BucketDay
	if s := q.Get("bucket"); s != "" {
		bucket = store.StatsBucket(s)
		switch bucket {
		case store.BucketHour, store.BucketDay, store.BucketWeek:
		default:
			http.Error(w, "invalid bucket", http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	summary, err := h.store.Summary(ctx, from, to)
	if err != nil {
		h.logger.Warn("stats summary", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	buckets, err := h.store.BucketSeries(ctx, from, to, bucket)
	if err != nil {
		h.logger.Warn("stats buckets", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	byRepo, err := h.store.PerRepo(ctx, from, to, 50)
	if err != nil {
		h.logger.Warn("stats per repo", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	byModel, err := h.store.PerModel(ctx, from, to)
	if err != nil {
		h.logger.Warn("stats per model", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, StatsResponse{
		From:    from,
		To:      to,
		Bucket:  bucket,
		Summary: summary,
		Buckets: buckets,
		ByRepo:  byRepo,
		ByModel: byModel,
	})
}

// telemetryRequest is the body of POST /api/runs/:id/telemetry.
// Field names mirror the OpenCode step_finish event shape so
// the runner's JSON unmarshals into this struct without a
// hand-written adapter.
//
// QUB-105: every new field the runner forwards is optional on
// the wire (the runner's pre-QUB-105 contract did not include
// them). Optional-with-pointer is the convention so a partial
// payload (older runner, network truncation) does not silently
// land zero where the SDK actually reported a value. The
// store's INSERT OR REPLACE fills the SQL DEFAULT for the
// absent scalar fields; the nullable TEXT / INTEGER columns
// stay NULL when the runner did not forward them.
type telemetryRequest struct {
	Model                 string  `json:"model"`
	Provider              string  `json:"provider,omitempty"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	ReasoningTokens       int64   `json:"reasoning_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheWriteTokens      int64   `json:"cache_write_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	CostPromptUSD         float64 `json:"cost_prompt_usd"`
	CostCompletionUSD     float64 `json:"cost_completion_usd"`
	CostUpstreamUSD       float64 `json:"cost_upstream_usd"`
	IsByok                bool    `json:"is_byok"`
	ServerToolCallsExec   int64   `json:"server_tool_calls_executed"`
	ServerToolCallsReq    int64   `json:"server_tool_calls_requested"`
	RequestID             *string `json:"request_id,omitempty"`
	DurationMS            *int64  `json:"duration_ms,omitempty"`
	StepCount             int     `json:"step_count"`
	Error                 *string `json:"error,omitempty"`
	ErrorStatusCode       *int64  `json:"error_status_code,omitempty"`
	ErrorContentType      *string `json:"error_content_type,omitempty"`
	ErrorBody             *string `json:"error_body,omitempty"`
}

// RecordTelemetry handles POST /api/runs/:id/telemetry. The
// runner posts once at the end of a review with the accumulated
// token usage and cost. We REPLACE any existing row (a re-run
// or re-delivery of the same run should land on the same row).
//
// Auth: X-BOOP-Runner-Token must match Config.RunnerToken. A
// missing token or non-matching token returns 401. We use
// constant-time compare to avoid timing leaks.
func (h *Handler) RecordTelemetry(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.checkRunnerToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	var body telemetryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	if err := h.store.RecordTelemetry(r.Context(), store.Telemetry{
		RunID:               id,
		Model:               body.Model,
		Provider:            body.Provider,
		InputTokens:         body.InputTokens,
		OutputTokens:        body.OutputTokens,
		TotalTokens:         body.TotalTokens,
		ReasoningTokens:     body.ReasoningTokens,
		CacheReadTokens:     body.CacheReadTokens,
		CacheWriteTokens:    body.CacheWriteTokens,
		CostUSD:             body.CostUSD,
		CostPromptUSD:       body.CostPromptUSD,
		CostCompletionUSD:   body.CostCompletionUSD,
		CostUpstreamUSD:     body.CostUpstreamUSD,
		IsByok:              body.IsByok,
		ServerToolCallsExec: body.ServerToolCallsExec,
		ServerToolCallsReq:  body.ServerToolCallsReq,
		RequestID:           body.RequestID,
		DurationMS:          body.DurationMS,
		StepCount:           body.StepCount,
		Error:               body.Error,
		ErrorStatusCode:     body.ErrorStatusCode,
		ErrorContentType:    body.ErrorContentType,
		ErrorBody:           body.ErrorBody,
	}); err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			// QUB-101: the store's default path is now to
			// INSERT OR IGNORE a placeholder run row, so this
			// branch only fires if a caller asked for the old
			// strict behaviour. Match RecordStatus's 202 so
			// the runner does not treat this as a hard
			// failure.
			h.logger.Info("record telemetry: unknown run, runner should retry on next stage", "run", id)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.logger.Warn("record telemetry", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// statusRequest is the body of POST /api/runs/:id/status. The
// runner posts one of these at each stage transition so the
// dashboard's "live runs" panel can show the current stage
// without polling K8s.
type statusRequest struct {
	Stage      string     `json:"stage"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMS *int64     `json:"duration_ms,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// RecordStatus handles POST /api/runs/:id/status. The runner
// posts at each stage so the dashboard's live view does not
// have to poll K8s (the Job is GC'd 1h after finish, so a
// dashboard read that lands later than that would see nothing
// without this row).
//
// Auth is the same X-BOOP-Runner-Token shared with telemetry.
func (h *Handler) RecordStatus(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.checkRunnerToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	var body statusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	st, ok := parseRunStage(body.Stage)
	if !ok {
		http.Error(w, "invalid stage", http.StatusBadRequest)
		return
	}
	_, err := h.store.UpdateRunStatus(r.Context(), id, st, body.EndedAt, body.DurationMS, body.Error)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			// Runner started before the receiver committed
			// the row. The runner will retry on the next
			// stage transition. Return 202 so the runner
			// doesn't treat this as a hard failure.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.logger.Warn("record status", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseRunStage maps the runner's stage name to a RunStatus.
// The runner's stages (auth, clone, review, done, failed) are
// the receiver's existing StatusAuth/Clone/Review/Done/Failed
// constants, but they live in different packages; this map is
// the only place the cross-package translation lives so adding
// a new stage means editing one map.
func parseRunStage(s string) (store.RunStatus, bool) {
	switch strings.ToLower(s) {
	case "running":
		return store.StatusRunning, true
	case "succeeded", "done":
		return store.StatusSucceeded, true
	case "failed":
		return store.StatusFailed, true
	}
	return "", false
}

// checkRunnerToken compares the request's X-BOOP-Runner-Token
// against h.cfg.RunnerToken using a constant-time compare. An
// empty Config.RunnerToken rejects every request — the
// receiver never accepts a runner POST unless the operator
// opted in by setting the env var.
func (h *Handler) checkRunnerToken(r *http.Request) bool {
	if h.cfg.RunnerToken == "" {
		return false
	}
	got := r.Header.Get("X-BOOP-Runner-Token")
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.cfg.RunnerToken)) == 1
}

// stageRequest is the body of POST /api/runs/:id/stages.
// The runner POSTs at each stage transition; the receiver
// stamps the row with the server's clock so the waterfall
// is on one clock across all stages (Phase 2's load-bearing
// correctness rule).
//
// The runner MAY send client_started_at; it is intentionally
// ignored. The only thing the runner contributes is the stage
// name, the meta blob, and the "this is the end of the stage"
// signal (ended=true). The receiver's clock is authoritative
// for both started_at and ended_at.
type stageRequest struct {
	Stage string `json:"stage"`
	Ended bool   `json:"ended,omitempty"`
	Meta  string `json:"meta,omitempty"`
}

// RecordStage handles POST /api/runs/:id/stages. The runner
// fires this at every stage transition; the receiver
// stamps the row with its own clock so the dashboard's
// waterfall is consistent across stages that span pods
// (hmac_verify runs in the receiver, pod_schedule runs in
// the K8s API, comment_post runs in the runner — they all
// need to be on the same wall clock for the bars to line
// up).
//
// Auth is the same X-BOOP-Runner-Token shared with the
// other runner endpoints.
func (h *Handler) RecordStage(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.checkRunnerToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	var body stageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Stage == "" {
		http.Error(w, "stage is required", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	stage := store.RunStage{
		RunID:     id,
		Stage:     body.Stage,
		StartedAt: now,
	}
	if body.Ended {
		// QU B-113 / EH-003: do NOT stamp duration_ms.
		// The previous shape set dur := int64(0) and
		// passed it through, which the SQL
		// ON CONFLICT clause happily wrote over the
		// real duration (0 is non-null, so
		// COALESCE(excluded, existing) takes 0). The
		// dashboard's durMS() falls back to
		// EndedAt - StartedAt when DurationMS is nil,
		// so the bar's real length renders correctly.
		// EH-001: a not-yet-persisted run triggers a
		// FK violation (run_stages.run_id REFERENCES
		// runs.id). The runner's postWithRetry
		// retries 5xx once, then drops the call —
		// the waterfall silently loses the start
		// POST. Return 202 here so the runner treats
		// it like RecordStatus / RecordHeartbeat
		// (a transient "run not visible yet" that the
		// next stage transition retries).
		stage.EndedAt = &now
	}
	if _, err := h.store.UpsertRunStage(r.Context(), stage); err != nil {
		if errors.Is(err, sql.ErrNoRows) || isForeignKeyError(err) {
			h.logger.Info("record stage: run not yet persisted, runner should retry on next stage", "run", id, "stage", body.Stage)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.logger.Warn("record stage", "run", id, "stage", body.Stage, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RecordHeartbeat handles POST /api/runs/:id/heartbeat.
// The runner posts every 30s while a review is in flight;
// the receiver updates runs.last_heartbeat_at to now (its
// own clock) and the stuck-runs panel reads the gap. A
// 2-minute gap with status=running = "stuck".
//
// Auth is the same X-BOOP-Runner-Token.
//
// 202 (run not yet persisted) is returned for a run the
// receiver hasn't seen yet — the runner will retry on the
// next tick. 204 on success.
func (h *Handler) RecordHeartbeat(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.checkRunnerToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	if err := h.store.TouchRunHeartbeat(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.logger.Warn("record heartbeat", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lensTelemetryRequest is the body of POST
// /api/runs/:id/lens_telemetry. The runner posts an array
// of per-lens rollups at the end of a run; the receiver
// records each row with the server's clock for started_at
// consistency.
//
// The Lens field is the lens name (security, deep, style,
// etc.); Model/Provider mirror the aggregate telemetry's
// shape so the dashboard can render one row per lens
// without joining. Tokens and CostUSD are this lens's
// contribution; the aggregate telemetry row stores the
// total.
type lensTelemetryRequest struct {
	Lens             string  `json:"lens"`
	Model            string  `json:"model,omitempty"`
	Provider         string  `json:"provider,omitempty"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	StepCount        int     `json:"step_count"`
}

// lensTelemetryBatchRequest is the body of POST
// /api/runs/:id/lens_telemetry. The runner accumulates per-
// lens rollups across the run and posts them as a single
// batch at end-of-run. The array is replaced atomically
// (DELETE + INSERT) so a re-run lands on the same shape
// the dashboard expects.
type lensTelemetryBatchRequest struct {
	Lenses []lensTelemetryRequest `json:"lenses"`
}

// RecordLensTelemetry handles POST /api/runs/:id/lens_telemetry.
// The runner parses `lens: <name>` markers from the
// orchestrator's output and POSTs one row per lens. The
// receiver REPLACES the per-lens rows for the run so a
// re-run / re-delivery lands on the same shape the
// dashboard expects.
//
// Auth is the same X-BOOP-Runner-Token.
func (h *Handler) RecordLensTelemetry(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.checkRunnerToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	var body lensTelemetryBatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rows := make([]store.LensTelemetry, 0, len(body.Lenses))
	for _, l := range body.Lenses {
		if l.Lens == "" {
			http.Error(w, "lens is required", http.StatusBadRequest)
			return
		}
		rows = append(rows, store.LensTelemetry{
			RunID:            id,
			Lens:             l.Lens,
			Model:            l.Model,
			Provider:         l.Provider,
			InputTokens:      l.InputTokens,
			OutputTokens:     l.OutputTokens,
			ReasoningTokens:  l.ReasoningTokens,
			CacheReadTokens:  l.CacheReadTokens,
			CacheWriteTokens: l.CacheWriteTokens,
			CostUSD:          l.CostUSD,
			StepCount:        l.StepCount,
		})
	}
	if err := h.store.ReplaceLensTelemetry(r.Context(), id, rows); err != nil {
		// EH-002: a not-yet-persisted run triggers a FK
		// violation (lens_telemetry.run_id REFERENCES
		// runs.id). Mirror RecordStage / RecordStatus's
		// 202 fallback so the runner's retry loop
		// catches up on the next stage transition
		// instead of losing the cost row. Without this,
		// a fast-starting run loses the entire
		// per-lens rollup (the 5xx-then-drop chain in
		// postWithRetry is silent in operator logs).
		if errors.Is(err, sql.ErrNoRows) || isForeignKeyError(err) {
			h.logger.Info("record lens telemetry: run not yet persisted, runner should retry on next stage", "run", id)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.logger.Warn("record lens telemetry", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isForeignKeyError reports whether err is a SQLite
// "FOREIGN KEY constraint failed" error. The error
// message is the only stable signal (sqlite3 does not
// export typed errors for FK violations); matching the
// substring is the same approach the rest of the store
// uses for parse-error detection. A driver swap would
// need to add a typed wrapper here; today the
// dependency on mattn/go-sqlite3 keeps the message
// stable.
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// startInstallationsPoller kicks off a background goroutine that
// refreshes the installations table from GitHub on a fixed
// interval. The poller is best-effort: a transient GitHub API
// error is logged and the previous cached data is left in
// place. The handler returns a pointer to a stop function so
// main can shut the poller down on signal.
//
// interval is clamped to a minimum of 1 minute to avoid an
// unbounded-tight loop on a misconfigured 0 or negative value.
func (h *Handler) StartInstallationsPoller(ctx context.Context, interval time.Duration) func() {
	if h.store == nil || h.ghClient == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		// First tick after a small delay so the receiver has
		// time to bind its port before we start hammering
		// GitHub.
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			if err := h.refreshInstallations(pollerCtx); err != nil {
				h.logger.Warn("installations poll", "err", err)
			}
			t.Reset(interval)
		}
	}()
	return cancel
}

func (h *Handler) refreshInstallations(ctx context.Context) error {
	fresh, err := h.ghClient.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	installs := make([]store.Installation, len(fresh))
	for i, ins := range fresh {
		installs[i] = store.Installation{
			ID:                  ins.ID,
			AccountLogin:        ins.AccountLogin,
			AccountType:         ins.AccountType,
			RepositorySelection: ins.RepositorySelection,
			InstalledAt:         ins.InstalledAt,
		}
	}
	if err := h.store.UpsertInstallations(ctx, installs); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	h.logger.Info("installations refreshed", "count", len(installs))
	return nil
}

// StartRetentionLoop kicks off the periodic cleanup pass that
// prunes old runs, runs a WAL checkpoint, and (weekly) runs
// incremental_vacuum. The loop is best-effort: each tick
// runs independently and a transient error is logged and
// swallowed. The returned cancel func stops the goroutine
// (safe to call multiple times).
//
// retention is the time window before "now" used as the
// PruneRuns cutoff (0 = store.DefaultRetention, 365 days).
// cleanupEvery is the tick period (0 = store.DefaultCleanupEvery,
// 5 min). vacuumInterval is the minimum time between
// incremental_vacuum calls (0 = store.DefaultVacuumInterval, 7
// days). The receiver logs the resolved values on startup so
// an operator can see the effective schedule.
func (h *Handler) StartRetentionLoop(ctx context.Context, retention, cleanupEvery, vacuumInterval time.Duration) func() {
	if h.store == nil {
		return func() {}
	}
	if retention <= 0 {
		retention = store.DefaultRetention
	}
	if cleanupEvery <= 0 {
		cleanupEvery = store.DefaultCleanupEvery
	}
	if vacuumInterval <= 0 {
		vacuumInterval = store.DefaultVacuumInterval
	}
	// Floor the cleanup tick at 30s to keep a misconfigured
	// zero/negative value from busy-looping the receiver.
	if cleanupEvery < 30*time.Second {
		cleanupEvery = 30 * time.Second
	}
	h.logger.Info("retention loop starting",
		"retention", retention,
		"cleanup_every", cleanupEvery,
		"vacuum_interval", vacuumInterval,
	)
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		// First tick after a small delay so the receiver has
		// time to bind its port and serve the readiness
		// probe before we start hammering SQLite.
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			tickCtx, tickCancel := context.WithTimeout(pollerCtx, 2*time.Minute)
			if _, err := h.store.RunRetention(tickCtx, retention, vacuumInterval); err != nil {
				h.logger.Warn("retention tick failed", "err", err)
			}
			tickCancel()
			t.Reset(cleanupEvery)
		}
	}()
	return cancel
}

// StartBackupLoop kicks off the periodic snapshot pass. Each
// tick writes a daily VACUUM-INTO snapshot to dir and prunes
// older entries. The receiver is one replica and the data
// PVC is RWO, so the backup has to happen in-process — a
// separate CronJob would not be able to mount the same PVC
// while the receiver holds it. The trade-off is that the
// backup is only as fresh as the receiver is alive; for
// point-in-time restore on a dead receiver, restore from the
// most recent snapshot and accept the gap.
//
// dir is the destination directory (typically /backups,
// backed by the boop-receiver-backups PVC). Empty dir
// disables the loop. every is the period (0 = 24h). keep is
// the number of daily snapshots to retain (0 = 30). All
// defaults live in the store package; this method just
// forwards them.
//
// The returned cancel func is safe to call multiple times.
// First tick is delayed by 15s so the receiver has time to
// serve the readiness probe and bind /backups before the
// first VACUUM-INTO call.
func (h *Handler) StartBackupLoop(ctx context.Context, dir string, every time.Duration, keep int) func() {
	if h.store == nil || dir == "" {
		return func() {}
	}
	if every <= 0 {
		every = store.DefaultBackupEvery
	}
	if keep <= 0 {
		keep = store.DefaultBackupKeep
	}
	h.logger.Info("backup loop starting", "dir", dir, "every", every, "keep", keep)
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			tickCtx, tickCancel := context.WithTimeout(pollerCtx, 10*time.Minute)
			if err := h.store.RunBackup(tickCtx, dir, keep); err != nil {
				h.logger.Warn("backup tick failed", "err", err)
			}
			tickCancel()
			t.Reset(every)
		}
	}()
	return cancel
}
