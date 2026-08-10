package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"github.com/michaelruelas/boop-receiver/internal/store"
)

// newTestHandlerWithStore builds a Handler with the same shape
// main.go builds: a fake kube client, a real (file-backed)
// store, and a stub ghClient. Tests can override the ghClient
// after construction if they need to stub ListInstallations.
func newTestHandlerWithStore(t *testing.T) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boop.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			RunnerToken:     "test-runner-token",
		},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		store:  s,
	}
}

func TestListInstallations_Empty(t *testing.T) {
	h := newTestHandlerWithStore(t)
	req := httptest.NewRequest("GET", "/api/installations", nil)
	rr := httptest.NewRecorder()
	h.ListInstallations(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp InstallationsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Installations) != 0 {
		t.Errorf("empty store: got %d", len(resp.Installations))
	}
}

func TestListInstallations_ReturnsStore(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	installs := []store.Installation{
		{ID: 1, AccountLogin: "alice", AccountType: "User", RepositorySelection: "all", FetchedAt: now},
		{ID: 2, AccountLogin: "org-b", AccountType: "Organization", RepositorySelection: "selected", FetchedAt: now},
	}
	if err := h.store.UpsertInstallations(ctx, installs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/installations", nil)
	rr := httptest.NewRecorder()
	h.ListInstallations(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp InstallationsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Installations) != 2 {
		t.Fatalf("len = %d, want 2", len(resp.Installations))
	}
	// Sorted by LOWER(account_login) ASC: alice < org-b.
	if resp.Installations[0].AccountLogin != "alice" {
		t.Errorf("sort: first = %q", resp.Installations[0].AccountLogin)
	}
}

func TestListInstallations_NoStoreReturns503(t *testing.T) {
	h := &Handler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest("GET", "/api/installations", nil)
	rr := httptest.NewRecorder()
	h.ListInstallations(rr, req)
	if rr.Code != 503 {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestListRuns_FilterByOwner(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	seeds := []store.Run{
		{ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1, CommitSHA: "aaaaaaa", Status: store.StatusSucceeded, StartedAt: base},
		{ID: "boop-a-b-2-bbbbbbb", Owner: "a", Repo: "b", PRNumber: 2, CommitSHA: "bbbbbbb", Status: store.StatusSucceeded, StartedAt: base.Add(time.Minute)},
		{ID: "boop-x-y-1-ccccccc", Owner: "x", Repo: "y", PRNumber: 1, CommitSHA: "ccccccc", Status: store.StatusSucceeded, StartedAt: base.Add(2 * time.Minute)},
	}
	for _, r := range seeds {
		if _, err := h.store.UpsertRun(ctx, r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	req := httptest.NewRequest("GET", "/api/runs?owner=a", nil)
	rr := httptest.NewRecorder()
	h.ListRuns(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp ListRunsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 2 {
		t.Errorf("len = %d, want 2", len(resp.Runs))
	}
}

func TestStats_BasicShape(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		_, err := h.store.UpsertRun(ctx, store.Run{
			ID:        "boop-a-b-" + string(rune('1'+i)) + "-aaaaaaa",
			Owner:     "a",
			Repo:      "b",
			PRNumber:  i + 1,
			CommitSHA: "aaaaaaa",
			Status:    store.StatusSucceeded,
			StartedAt: base.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	req := httptest.NewRequest("GET", "/api/stats", nil)
	rr := httptest.NewRecorder()
	h.Stats(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp StatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Summary.TotalRuns != 3 {
		t.Errorf("total = %d, want 3", resp.Summary.TotalRuns)
	}
	if len(resp.ByRepo) != 1 {
		t.Errorf("by_repo len = %d, want 1", len(resp.ByRepo))
	}
}

func TestRecordTelemetry_HappyPath(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusSucceeded,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{
		"model": "openrouter/anthropic/claude-3.5-sonnet",
		"provider": "openrouter",
		"input_tokens": 1000,
		"output_tokens": 500,
		"cost_usd": 0.0123,
		"step_count": 3
	}`
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	telem, err := h.store.GetTelemetry(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if telem.CostUSD != 0.0123 {
		t.Errorf("cost = %f", telem.CostUSD)
	}
}

func TestRecordTelemetry_QUB105Fields(t *testing.T) {
	// QUB-105 acceptance: the receiver reads every QUB-105
	// field, persists them, and surfaces them through
	// GetTelemetry. The fixture exercises the full set:
	// total_tokens, the cost split, is_byok, server tool
	// stats, request_id, duration_ms, and the failed-call
	// error context.
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusSucceeded,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{
		"model": "openrouter/anthropic/claude-3.5-sonnet",
		"provider": "openrouter",
		"input_tokens": 1000,
		"output_tokens": 500,
		"total_tokens": 1505,
		"reasoning_tokens": 5,
		"cache_read_tokens": 200,
		"cache_write_tokens": 0,
		"cost_usd": 0.0123,
		"cost_prompt_usd": 0.001,
		"cost_completion_usd": 0.0113,
		"cost_upstream_usd": 0.0124,
		"is_byok": true,
		"server_tool_calls_executed": 0,
		"server_tool_calls_requested": 0,
		"request_id": "chatcmpl-xyz",
		"duration_ms": 4321,
		"step_count": 3
	}`
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	telem, err := h.store.GetTelemetry(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if telem.TotalTokens != 1505 {
		t.Errorf("total_tokens = %d", telem.TotalTokens)
	}
	if telem.CostPromptUSD != 0.001 || telem.CostCompletionUSD != 0.0113 || telem.CostUpstreamUSD != 0.0124 {
		t.Errorf("cost split = (%f, %f, %f)", telem.CostPromptUSD, telem.CostCompletionUSD, telem.CostUpstreamUSD)
	}
	if !telem.IsByok {
		t.Errorf("is_byok = false, want true")
	}
	if telem.RequestID == nil || *telem.RequestID != "chatcmpl-xyz" {
		t.Errorf("request_id = %v", telem.RequestID)
	}
	if telem.DurationMS == nil || *telem.DurationMS != 4321 {
		t.Errorf("duration_ms = %v", telem.DurationMS)
	}
}

func TestRecordTelemetry_QUB105ErrorContext(t *testing.T) {
	// QUB-105: a failed-call telemetry row stamps error (the
	// human-readable message), error_status_code,
	// error_content_type, and error_body (the SDK response
	// snippet) so a 4xx is diagnosable from the dashboard
	// without digging through pod logs. The handler forwards
	// the JSON fields to the store; the store persists them
	// and round-trips them through GetTelemetry.
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusFailed,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := `{
		"model": "openrouter/x",
		"step_count": 1,
		"error": "OpenRouter chat completion failed (401): Bad token",
		"error_status_code": 401,
		"error_content_type": "application/json",
		"error_body": "{\"error\":\"unauthorized\"}"
	}`
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	telem, err := h.store.GetTelemetry(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if telem.Error == nil || *telem.Error != "OpenRouter chat completion failed (401): Bad token" {
		t.Errorf("error = %v", telem.Error)
	}
	if telem.ErrorStatusCode == nil || *telem.ErrorStatusCode != 401 {
		t.Errorf("error_status_code = %v", telem.ErrorStatusCode)
	}
	if telem.ErrorContentType == nil || *telem.ErrorContentType != "application/json" {
		t.Errorf("error_content_type = %v", telem.ErrorContentType)
	}
	if telem.ErrorBody == nil || *telem.ErrorBody != `{"error":"unauthorized"}` {
		t.Errorf("error_body = %v", telem.ErrorBody)
	}
}

func TestRecordTelemetry_Auth(t *testing.T) {
	h := newTestHandlerWithStore(t)
	body := `{"model": "x", "input_tokens": 0, "output_tokens": 0, "cost_usd": 0, "step_count": 0}`

	// Missing token
	rr := doRequest(t, h, "POST", "/api/runs/x/telemetry", body, nil)
	if rr.Code != 401 {
		t.Errorf("missing token: status = %d, want 401", rr.Code)
	}

	// Wrong token
	rr = doRequest(t, h, "POST", "/api/runs/x/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "wrong"})
	if rr.Code != 401 {
		t.Errorf("wrong token: status = %d, want 401", rr.Code)
	}

	// Empty token configured
	h.cfg.RunnerToken = ""
	rr = doRequest(t, h, "POST", "/api/runs/x/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "anything"})
	if rr.Code != 401 {
		t.Errorf("empty server token: status = %d, want 401", rr.Code)
	}
}

func TestRecordTelemetry_UnknownRun(t *testing.T) {
	// QUB-101: a runner POST that lands before the receiver's
	// UpsertRun has committed gets ErrUnknownRun back from the
	// store, which the handler matches and turns into a 202.
	// Matches RecordStatus's behavior for the same condition.
	h := newTestHandlerWithStore(t)
	body := `{"model": "x", "input_tokens": 0, "output_tokens": 0, "cost_usd": 0, "step_count": 0}`
	rr := doRequest(t, h, "POST", "/api/runs/nonexistent/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rr.Code)
	}
}

func TestRecordStatus_HappyPath(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusRunning,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{"stage": "succeeded"}`
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/status", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	run, err := h.store.GetRun(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if run.Status != store.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", run.Status)
	}
}

func TestRecordStatus_UnknownRunReturns202(t *testing.T) {
	h := newTestHandlerWithStore(t)
	body := `{"stage": "succeeded"}`
	rr := doRequest(t, h, "POST", "/api/runs/nonexistent/status", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	// 202 (not 404) — the runner will retry on the next stage
	// transition. Treating it as 404 would force the runner
	// to log + skip telemetry for the rest of the run.
	if rr.Code != 202 {
		t.Errorf("status = %d, want 202", rr.Code)
	}
}

// CQ-005: RecordStage is the load-bearing endpoint for
// the dashboard's waterfall. The start POST stamps
// started_at; the end POST stamps ended_at and the
// dashboard's durMS() derives DurationMS from the
// EndedAt - StartedAt delta (the handler MUST NOT
// stamp duration_ms = 0 — EH-003). The runner posts
// both shapes for every stage; pinning both here
// guards against the regression of the handler
// regressing to "always set duration_ms".
func TestRecordStage_StartAndEndStampsClock(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Start POST.
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/stages", `{"stage":"clone","meta":"{\"path\":\"x\"}"}`, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("start status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// End POST.
	rr = doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/stages", `{"stage":"clone","ended":true}`, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("end status = %d, body = %s", rr.Code, rr.Body.String())
	}
	stages, err := h.store.ListRunStages(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(stages))
	}
	got := stages[0]
	if got.Stage != "clone" {
		t.Errorf("stage = %q, want clone", got.Stage)
	}
	if got.StartedAt.IsZero() {
		t.Errorf("started_at zero")
	}
	if got.EndedAt == nil {
		t.Fatalf("ended_at nil")
	}
	if got.EndedAt.Before(got.StartedAt) {
		t.Errorf("ended_at before started_at")
	}
	// EH-003: DurationMS MUST remain nil on the end POST.
	// The dashboard's durMS() falls back to
	// EndedAt - StartedAt when DurationMS is nil. A
	// handler that stamps duration_ms=0 on every end
	// POST clobbers the real duration via the SQL
	// COALESCE(excluded, existing) and the waterfall
	// renders 0ms bars regardless of how long the
	// stage actually took.
	if got.DurationMS != nil {
		t.Errorf("duration_ms = %d, want nil (dashboard computes from EndedAt-StartedAt)", *got.DurationMS)
	}
}

// CQ-005: a stage POST for a run the receiver has
// not yet persisted (the runner raced ahead of the
// webhook) returns 202, not 500. The runner's
// postWithRetry drops 5xx, so a 500 here loses the
// stage row entirely. This is the same shape as
// TestRecordStatus_UnknownRunReturns202 and pins
// the same contract for stages.
func TestRecordStage_UnknownRunReturns202(t *testing.T) {
	h := newTestHandlerWithStore(t)
	rr := doRequest(t, h, "POST", "/api/runs/nonexistent/stages", `{"stage":"clone"}`, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 202 {
		t.Errorf("status = %d, want 202", rr.Code)
	}
}

// CQ-005: RecordHeartbeat updates last_heartbeat_at
// on the run row. The dashboard's "stuck" panel
// reads the gap; a heartbeat that 500s (instead of
// 202) would silently break the stuck-run detection
// for a not-yet-persisted run.
func TestRecordHeartbeat_HappyPath(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/heartbeat", "", map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	run, err := h.store.GetRun(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if run.LastHeartbeatAt == nil {
		t.Errorf("last_heartbeat_at nil after heartbeat")
	}
}

func TestRecordHeartbeat_UnknownRunReturns202(t *testing.T) {
	h := newTestHandlerWithStore(t)
	rr := doRequest(t, h, "POST", "/api/runs/nonexistent/heartbeat", "", map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 202 {
		t.Errorf("status = %d, want 202", rr.Code)
	}
}

// CQ-005: RecordLensTelemetry replaces the per-lens
// rows for the run. The dashboard's "Costs & lenses"
// view reads this; a 500 here (for a not-yet-persisted
// run) would drop the entire cost rollup silently.
func TestRecordLensTelemetry_HappyPath(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", Status: store.StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := `{"lenses":[
		{"lens":"security","model":"openrouter/anthropic/claude-3.5-sonnet","input_tokens":100,"output_tokens":50,"cost_usd":0.01,"step_count":1},
		{"lens":"deep","model":"openrouter/anthropic/claude-3.5-sonnet","input_tokens":200,"output_tokens":100,"cost_usd":0.02,"step_count":2}
	]}`
	rr := doRequest(t, h, "POST", "/api/runs/boop-a-b-1-aaaaaaa/lens_telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 204 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	lenses, err := h.store.ListLensTelemetry(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lenses) != 2 {
		t.Fatalf("lenses = %d, want 2", len(lenses))
	}
}

func TestRecordLensTelemetry_UnknownRunReturns202(t *testing.T) {
	h := newTestHandlerWithStore(t)
	body := `{"lenses":[{"lens":"security","input_tokens":100,"output_tokens":50,"cost_usd":0.01,"step_count":1}]}`
	rr := doRequest(t, h, "POST", "/api/runs/nonexistent/lens_telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 202 {
		t.Errorf("status = %d, want 202", rr.Code)
	}
}

// doRequest routes a request through the same ServeMux the
// receiver uses in main.go so r.PathValue("id") is populated
// for the {id}-style paths. The alternative — calling the
// handler method directly — leaves PathValue empty and breaks
// the {id} routes.
func doRequest(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/installations", h.ListInstallations)
	mux.HandleFunc("GET /api/runs", h.ListRuns)
	mux.HandleFunc("GET /api/stats", h.Stats)
	mux.HandleFunc("POST /api/runs/{id}/telemetry", h.RecordTelemetry)
	mux.HandleFunc("POST /api/runs/{id}/status", h.RecordStatus)
	mux.HandleFunc("POST /api/runs/{id}/stages", h.RecordStage)
	mux.HandleFunc("POST /api/runs/{id}/heartbeat", h.RecordHeartbeat)
	mux.HandleFunc("POST /api/runs/{id}/lens_telemetry", h.RecordLensTelemetry)

	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestParseRunStage(t *testing.T) {
	cases := []struct {
		in   string
		want store.RunStatus
		ok   bool
	}{
		{"running", store.StatusRunning, true},
		{"succeeded", store.StatusSucceeded, true},
		{"done", store.StatusSucceeded, true},
		{"failed", store.StatusFailed, true},
		{"FAILED", store.StatusFailed, true},
		{"unknown", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := parseRunStage(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseRunStage(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestListRuns_StatusFilter(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, s := range []store.RunStatus{store.StatusSucceeded, store.StatusFailed, store.StatusRunning} {
		if _, err := h.store.UpsertRun(ctx, store.Run{
			ID: "boop-a-b-" + string(rune('1'+i)) + "-aaaaaaa",
			Owner: "a", Repo: "b", PRNumber: i + 1, CommitSHA: "aaaaaaa",
			Status: s, StartedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	req := httptest.NewRequest("GET", "/api/runs?status=failed", nil)
	rr := httptest.NewRecorder()
	h.ListRuns(rr, req)
	var resp ListRunsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("len = %d, want 1", len(resp.Runs))
	}
	if resp.Runs[0].Run.Status != store.StatusFailed {
		t.Errorf("status = %q, want failed", resp.Runs[0].Run.Status)
	}
}

// TestStartInstallationsPoller_RunsImmediately ensures the
// background poller kicks off a refresh when invoked. We don't
// stub the ghClient here; the test just confirms the goroutine
// starts and is cancellable.
func TestStartInstallationsPoller_Cancelable(t *testing.T) {
	h := newTestHandlerWithStore(t)
	stop := h.StartInstallationsPoller(context.Background(), time.Hour)
	if stop == nil {
		t.Fatal("nil stop fn")
	}
	stop()
}

// Sanity check: the ghClient's Installation JSON tags are the
// shape the dashboard expects. compile-time-ish.
func TestInstallation_WireShape(t *testing.T) {
	ins := boopgithub.Installation{
		ID: 1, AccountLogin: "a", AccountType: "User",
		InstalledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	b, _ := json.Marshal(ins)
	if !bytes.Contains(b, []byte(`"account_login":"a"`)) {
		t.Errorf("wire shape: %s", string(b))
	}
}

func TestGetRun_Found(t *testing.T) {
	h := newTestHandlerWithStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if _, err := h.store.UpsertRun(ctx, store.Run{
		ID: "boop-test-1-abc1234", Owner: "o", Repo: "r", PRNumber: 1,
		CommitSHA: "abc1234", Status: store.StatusSucceeded,
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs/{id}", h.GetRun)
	req := httptest.NewRequest("GET", "/api/runs/boop-test-1-abc1234", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp RunWithTelemetry
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "boop-test-1-abc1234" {
		t.Errorf("id = %q, want boop-test-1-abc1234", resp.ID)
	}
	if resp.Status != store.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", resp.Status)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	h := newTestHandlerWithStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs/{id}", h.GetRun)
	req := httptest.NewRequest("GET", "/api/runs/nonexistent", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
}

func TestGetRun_NoStore(t *testing.T) {
	h := &Handler{
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		store:  nil,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs/{id}", h.GetRun)
	req := httptest.NewRequest("GET", "/api/runs/x", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
