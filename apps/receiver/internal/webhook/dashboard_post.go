package webhook

// Dashboard POST endpoints (RF-007 split).
//
// Runner-only POST handlers. Every endpoint gates on
// X-BOOP-Runner-Token via checkRunnerToken (defined in
// dashboard.go). Each handler also handles the "run not
// yet persisted" race (QUB-101 / EH-001 / EH-002) by
// returning 202 — the runner's postWithRetry retries on
// the next stage transition so a fast-starting run
// lands the eventual cost / status row.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

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
	Model               string  `json:"model"`
	Provider            string  `json:"provider,omitempty"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheWriteTokens    int64   `json:"cache_write_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	CostPromptUSD       float64 `json:"cost_prompt_usd"`
	CostCompletionUSD   float64 `json:"cost_completion_usd"`
	CostUpstreamUSD     float64 `json:"cost_upstream_usd"`
	IsByok              bool    `json:"is_byok"`
	ServerToolCallsExec int64   `json:"server_tool_calls_executed"`
	ServerToolCallsReq  int64   `json:"server_tool_calls_requested"`
	RequestID           *string `json:"request_id,omitempty"`
	DurationMS          *int64  `json:"duration_ms,omitempty"`
	StepCount           int     `json:"step_count"`
	Error               *string `json:"error,omitempty"`
	ErrorStatusCode     *int64  `json:"error_status_code,omitempty"`
	ErrorContentType    *string `json:"error_content_type,omitempty"`
	ErrorBody           *string `json:"error_body,omitempty"`
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
		// QUB-113 / EH-003: do NOT stamp duration_ms.
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
