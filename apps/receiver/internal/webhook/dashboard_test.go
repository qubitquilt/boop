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
	dsn := "file:" + filepath.Join(t.TempDir(), "boop.db") + "?cache=shared&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	s, err := store.Open(dsn)
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
	h := newTestHandlerWithStore(t)
	body := `{"model": "x", "input_tokens": 0, "output_tokens": 0, "cost_usd": 0, "step_count": 0}`
	rr := doRequest(t, h, "POST", "/api/runs/nonexistent/telemetry", body, map[string]string{"X-BOOP-Runner-Token": "test-runner-token"})
	if rr.Code != 404 {
		t.Errorf("status = %d, want 404", rr.Code)
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
