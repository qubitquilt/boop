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

	page, err := h.store.ListRuns(r.Context(), f)
	if err != nil {
		h.logger.Warn("list runs", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	telemByRun := map[string]store.Telemetry{}
	for _, run := range page.Runs {
		if t, err := h.store.GetTelemetry(r.Context(), run.ID); err == nil {
			telemByRun[run.ID] = t
		}
	}

	out := make([]RunWithTelemetry, 0, len(page.Runs))
	for _, run := range page.Runs {
		out = append(out, RunWithTelemetry{Run: run, Telemetry: telemByRun[run.ID]})
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

// StatsResponse is the body of GET /api/stats. The shape is
// stable: summary is the top-line KPI block, buckets is the
// time-series, by_repo and by_model are the leaderboards. The
// dashboard renders this in one fetch.
type StatsResponse struct {
	From        time.Time                `json:"from"`
	To          time.Time                `json:"to"`
	Bucket      store.StatsBucket        `json:"bucket"`
	Summary     store.SummaryStats       `json:"summary"`
	Buckets     []store.BucketPoint      `json:"buckets"`
	ByRepo      []store.RepoRollup       `json:"by_repo"`
	ByModel     []store.ModelRollup      `json:"by_model"`
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
type telemetryRequest struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider,omitempty"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	StepCount        int     `json:"step_count"`
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
		RunID:            id,
		Model:            body.Model,
		Provider:         body.Provider,
		InputTokens:      body.InputTokens,
		OutputTokens:     body.OutputTokens,
		ReasoningTokens:  body.ReasoningTokens,
		CacheReadTokens:  body.CacheReadTokens,
		CacheWriteTokens: body.CacheWriteTokens,
		CostUSD:          body.CostUSD,
		StepCount:        body.StepCount,
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "unknown run", http.StatusNotFound)
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
	Stage     string     `json:"stage"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	DurationMS *int64    `json:"duration_ms,omitempty"`
	Error     string     `json:"error,omitempty"`
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
		if errors.Is(err, sql.ErrNoRows) {
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
