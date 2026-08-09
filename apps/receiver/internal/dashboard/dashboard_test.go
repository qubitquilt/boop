package dashboard

import (
	"context"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// newTestDashboard builds a Handler with a real (file-backed)
// store, a stub Actions.CreateRerunJob that records what
// the dashboard called it with (used by TestServeRerun*
// to assert the cross-package call), and a discard logger.
func newTestDashboard(t *testing.T, rerunFn func(ctx context.Context, prior store.Run, reason string) (string, error)) *Handler {
	t.Helper()
	return newTestDashboardWithLogs(t, rerunFn, nil)
}

// newTestDashboardWithLogs is the variant that also wires
// Actions.FetchPodLogs. Used by TestServeRunDetail* to
// assert that the dashboard surfaces the runner's pod logs.
func newTestDashboardWithLogs(t *testing.T, rerunFn func(ctx context.Context, prior store.Run, reason string) (string, error), fetchLogsFn func(ctx context.Context, jobName string) (string, error)) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boop.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Handler{
		store:  s,
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		token:  "test-dashboard-token",
		actions: Actions{
			CreateRerunJob: rerunFn,
			FetchPodLogs:   fetchLogsFn,
		},
	}
}

// seedRunForDashboard writes a single run row the dashboard
// tests can mutate. The id is fixed so the test bodies can
// stay terse; the caller chooses the status (success/fail/etc).
func seedRunForDashboard(t *testing.T, h *Handler, status store.RunStatus, cost float64) store.Run {
	t.Helper()
	run := store.Run{
		ID:             "boop-a-b-1-aaaaaaa",
		Owner:          "a",
		Repo:           "b",
		PRNumber:       1,
		CommitSHA:      "aaaaaaa1234567",
		BaseRef:        "main",
		ReviewNumber:   1,
		Reason:         "pull_request.opened",
		InstallationID: 12345,
		Status:         status,
		StartedAt:      time.Now().UTC(),
	}
	if _, err := h.store.UpsertRun(context.Background(), run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if cost > 0 {
		if err := h.store.RecordTelemetry(context.Background(), store.Telemetry{
			RunID:           run.ID,
			Model:           "openrouter/test",
			InputTokens:     100,
			OutputTokens:    50,
			ReasoningTokens: 25,
			CostUSD:         cost,
		}); err != nil {
			t.Fatalf("seed telemetry: %v", err)
		}
	}
	return run
}

// makeFormRequest builds the form-encoded body the dashboard
// routes expect. Browsers send application/x-www-form-urlencoded;
// the Go test client defaults to that when body is a url.Values.
func makeFormRequest(t *testing.T, method, target string, fields map[string]string) *http.Request {
	t.Helper()
	body := strings.Builder{}
	for k, v := range fields {
		if body.Len() > 0 {
			body.WriteByte('&')
		}
		body.WriteString(k + "=" + v)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body.String()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// TestServeRerun_HappyPath exercises the form-based Requeue
// flow on the exceptions view. Confirms the cross-package
// callback is called with the prior run, the reason is
// forwarded, and the response is a 303 to /dashboard/exceptions.
func TestServeRerun_HappyPath(t *testing.T) {
	var gotPrior store.Run
	var gotReason string
	d := newTestDashboard(t, func(_ context.Context, prior store.Run, reason string) (string, error) {
		gotPrior = prior
		gotReason = reason
		return prior.ID + "-r1", nil
	})
	seedRunForDashboard(t, d, store.StatusFailed, 0)

	req := makeFormRequest(t, "POST", "/dashboard/runs/boop-a-b-1-aaaaaaa/rerun",
		map[string]string{"confirm": "true", "reason": "manual"})
	rr := httptest.NewRecorder()
	d.serveRerun(rr, req, "boop-a-b-1-aaaaaaa")

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	if gotPrior.ID != "boop-a-b-1-aaaaaaa" {
		t.Errorf("callback prior.ID = %q, want %q", gotPrior.ID, "boop-a-b-1-aaaaaaa")
	}
	if gotReason != "manual" {
		t.Errorf("callback reason = %q, want %q", gotReason, "manual")
	}
	// Audit row should exist.
	events, err := d.store.ListAuditEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no audit row written")
	}
	if events[0].Action != "rerun.create" {
		t.Errorf("audit action = %q, want rerun.create", events[0].Action)
	}
	if !strings.HasPrefix(events[0].Actor, "dashboard:") {
		t.Errorf("audit actor = %q, want dashboard:* prefix", events[0].Actor)
	}
}

// TestServeRerun_RejectsNonTerminalPrior pins the boundary
// check — a still-running prior returns 409 without
// touching the cross-package callback.
func TestServeRerun_RejectsNonTerminalPrior(t *testing.T) {
	var called bool
	d := newTestDashboard(t, func(_ context.Context, _ store.Run, _ string) (string, error) {
		called = true
		return "", nil
	})
	seedRunForDashboard(t, d, store.StatusRunning, 0)

	req := makeFormRequest(t, "POST", "/dashboard/runs/boop-a-b-1-aaaaaaa/rerun",
		map[string]string{"confirm": "true", "reason": "test"})
	rr := httptest.NewRecorder()
	d.serveRerun(rr, req, "boop-a-b-1-aaaaaaa")

	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rr.Code)
	}
	if called {
		t.Error("cross-package callback was invoked on non-terminal prior")
	}
}

// TestServeRerun_RejectsMissingConfirm pins the CSRF
// defense — confirm=true must be present.
func TestServeRerun_RejectsMissingConfirm(t *testing.T) {
	d := newTestDashboard(t, func(_ context.Context, _ store.Run, _ string) (string, error) {
		t.Fatal("callback should not be called")
		return "", nil
	})
	seedRunForDashboard(t, d, store.StatusFailed, 0)

	req := makeFormRequest(t, "POST", "/dashboard/runs/boop-a-b-1-aaaaaaa/rerun",
		map[string]string{"reason": "missing confirm"})
	rr := httptest.NewRecorder()
	d.serveRerun(rr, req, "boop-a-b-1-aaaaaaa")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// TestServeRerun_NilCallbackReturns503 covers the
// read-only deploy that did not wire CreateRerunJob.
// The button still renders (the form action is
// server-side) but the POST returns 503 with a
// "not configured" message.
func TestServeRerun_NilCallbackReturns503(t *testing.T) {
	d := newTestDashboard(t, nil)
	d.actions.CreateRerunJob = nil
	seedRunForDashboard(t, d, store.StatusFailed, 0)

	req := makeFormRequest(t, "POST", "/dashboard/runs/boop-a-b-1-aaaaaaa/rerun",
		map[string]string{"confirm": "true", "reason": "test"})
	rr := httptest.NewRecorder()
	d.serveRerun(rr, req, "boop-a-b-1-aaaaaaa")

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// TestServeZeroCost_HappyPath: the "Zero out cost" form
// posts a refund row (the audit-trail side) and an audit
// event (the cross-cutting side). Both must land for the
// operator's later "did I really zero this?" question to
// have a single canonical answer.
func TestServeZeroCost_HappyPath(t *testing.T) {
	d := newTestDashboard(t, nil)
	seedRunForDashboard(t, d, store.StatusFailed, 0.0123)

	req := makeFormRequest(t, "POST", "/dashboard/runs/boop-a-b-1-aaaaaaa/zero-cost",
		map[string]string{"reason": "lens broken"})
	rr := httptest.NewRecorder()
	d.serveZeroCost(rr, req, "boop-a-b-1-aaaaaaa")

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	// Refund row landed.
	refunds, err := d.store.ListRefunds(context.Background(), "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("list refunds: %v", err)
	}
	if len(refunds) != 1 {
		t.Fatalf("got %d refunds, want 1", len(refunds))
	}
	// Tokens == input + output + reasoning.
	if refunds[0].Tokens != 100+50+25 {
		t.Errorf("refund tokens = %d, want 175", refunds[0].Tokens)
	}
	if refunds[0].RefundedBy == "" {
		t.Error("refunded_by empty")
	}
	// Audit row landed.
	events, _ := d.store.ListAuditEvents(context.Background(), 10)
	if len(events) != 1 || events[0].Action != "cost.zero_out" {
		t.Errorf("audit events = %d (action %q), want 1 cost.zero_out", len(events), events[0].Action)
	}
}

// TestServeZeroCost_NoTelemetryStillLands: a click on a
// never-started run row still records a refund row
// (zero tokens). The dashboard's "refund history" view
// should not silently swallow zero-out attempts on
// empty runs.
func TestServeZeroCost_NoTelemetryStillLands(t *testing.T) {
	d := newTestDashboard(t, nil)
	seedRunForDashboard(t, d, store.StatusFailed, 0) // no telemetry

	req := makeFormRequest(t, "POST", "/dashboard/runs/boop-a-b-1-aaaaaaa/zero-cost",
		map[string]string{"reason": ""})
	rr := httptest.NewRecorder()
	d.serveZeroCost(rr, req, "boop-a-b-1-aaaaaaa")

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	refunds, _ := d.store.ListRefunds(context.Background(), "boop-a-b-1-aaaaaaa")
	if len(refunds) != 1 {
		t.Fatalf("got %d refunds, want 1", len(refunds))
	}
	if refunds[0].Tokens != 0 {
		t.Errorf("refund tokens = %d, want 0", refunds[0].Tokens)
	}
}

// TestServeInstallationControl_PauseWritesAudit verifies
// the pause-toggle path writes the install.pause audit
// row. The store-side test (TestSetInstallationControls_
// AndPauseCheck in store_test.go) already pins the
// column update; this one pins the audit shape.
func TestServeInstallationControl_PauseWritesAudit(t *testing.T) {
	d := newTestDashboard(t, nil)
	if err := d.store.UpsertInstallations(context.Background(), []store.Installation{
		{ID: 1, AccountLogin: "alice", AccountType: "User", FetchedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	req := makeFormRequest(t, "POST", "/dashboard/installations/1",
		map[string]string{
			"paused":       "true",
			"lens_opt_out": "",
			"reason":       "noisy install",
		})
	rr := httptest.NewRecorder()
	d.serveInstallationControl(rr, req, "1")

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	events, _ := d.store.ListAuditEvents(context.Background(), 10)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Action != "install.pause" {
		t.Errorf("action = %q, want install.pause", events[0].Action)
	}
	if !strings.Contains(events[0].Details, "noisy install") {
		t.Errorf("details does not include reason: %q", events[0].Details)
	}
}

// TestServeInstallationControl_ResumeWritesAudit pins the
// resume-from-paused path: a prior paused row + a false
// paused value yields "install.resume", not "install.pause".
func TestServeInstallationControl_ResumeWritesAudit(t *testing.T) {
	d := newTestDashboard(t, nil)
	// Seed the install row first so SetInstallationControls
	// (the prior-pause step) actually affects a row. Without
	// this, the UPDATE affects 0 rows and the prev value in
	// serveInstallationControl is the zero-value (Paused=false),
	// which matches the form's paused=false — no transition
	// detected, no install.resume row.
	if err := d.store.UpsertInstallations(context.Background(), []store.Installation{
		{ID: 1, AccountLogin: "alice", AccountType: "User", FetchedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	if err := d.store.SetInstallationControls(context.Background(), 1, true, nil); err != nil {
		t.Fatalf("prior pause: %v", err)
	}

	req := makeFormRequest(t, "POST", "/dashboard/installations/1",
		map[string]string{"paused": "false", "lens_opt_out": ""})
	rr := httptest.NewRecorder()
	d.serveInstallationControl(rr, req, "1")

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	events, _ := d.store.ListAuditEvents(context.Background(), 10)
	if len(events) != 1 || events[0].Action != "install.resume" {
		t.Errorf("action = %q, want install.resume", events[0].Action)
	}
}

// TestActorFromToken exercises the audit-actor derivation.
// The two-part invariant is worth pinning: a stable
// token yields a stable actor, and an empty token yields
// the dashboard:disabled marker rather than (the
// string-equality trap) an empty string that fails the
// "non-empty actor" check in RecordAuditEvent.
func TestActorFromToken(t *testing.T) {
	t.Run("stable per token", func(t *testing.T) {
		d := &Handler{token: "abc"}
		first := d.actor()
		second := d.actor()
		if first != second {
			t.Errorf("actor drifted: %q vs %q", first, second)
		}
		if !strings.HasPrefix(first, "dashboard:") {
			t.Errorf("actor = %q, want dashboard: prefix", first)
		}
	})
	t.Run("disabled for empty", func(t *testing.T) {
		d := &Handler{token: ""}
		if got := d.actor(); got != "dashboard:disabled" {
			t.Errorf("empty token actor = %q, want dashboard:disabled", got)
		}
	})
}

// TestParseAllTemplates asserts every template in the
// embed parses cleanly. The dashboard's renderPage
// helper parses (layout, page) per request; if any
// template fails to parse, the corresponding route
// 500s. This test catches template syntax errors at
// `go test` time rather than at the next dashboard
// load.
func TestParseAllTemplates(t *testing.T) {
	for _, name := range []string{
		"layout.html",
		"runs.html",
		"run_detail.html",
		"live.html",
		"exceptions.html",
		"costs.html",
		"retention.html",
		"installations.html",
	} {
		if _, err := template.ParseFS(templateFS, "templates/"+name); err != nil {
			t.Errorf("parse %s: %v", name, err)
		}
	}
}

// TestServeRetention_RendersSchedule pins the new
// route's two-part contract: the handler returns 200,
// the response body contains the run id, and the
// default retention window is rendered in days. The
// "imminent" flag is exercised in TestServeRetention_
// ImminentFlag below.
func TestServeRetention_RendersSchedule(t *testing.T) {
	d := newTestDashboard(t, nil)
	run := seedRunForDashboard(t, d, store.StatusSucceeded, 0)

	req := httptest.NewRequest("GET", "/dashboard/retention", nil)
	rr := httptest.NewRecorder()
	d.serveRetention(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, run.ID) {
		t.Errorf("body missing run id %q; body=%q", run.ID, body)
	}
	if !strings.Contains(body, "365") {
		t.Errorf("body missing default retention days (365); body=%q", body)
	}
}

// TestServeRetention_ImminentFlag exercises the
// < 7 days branch. We backdate the run's started_at to
// 360 days ago so its scheduled deletion lands in 5
// days, which crosses the imminent threshold. A
// fresh-seeded run lands ~365 days out (NOT imminent),
// so the row's class attribute must NOT include "stuck".
//
// UpsertRun preserves started_at (it's an immutable
// timestamp for the receiver — the row's lifetime is
// anchored to the first POST). To backdate, we run
// a direct SQL UPDATE on the test DB. The dashboard
// only reads started_at, so this is a sufficient seam.
func TestServeRetention_ImminentFlag(t *testing.T) {
	d := newTestDashboard(t, nil)
	run := seedRunForDashboard(t, d, store.StatusSucceeded, 0)

	// Backdate started_at by 360 days via raw SQL.
	backdated := run.StartedAt.Add(-360 * 24 * time.Hour)
	res, err := d.store.DB().ExecContext(context.Background(),
		`UPDATE runs SET started_at = ? WHERE id = ?`,
		backdated.UTC().Format(time.RFC3339Nano), run.ID)
	if err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("backdate affected %d rows, want 1", n)
	}

	req := httptest.NewRequest("GET", "/dashboard/retention", nil)
	rr := httptest.NewRecorder()
	d.serveRetention(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// The retention.html template emits class="stuck"
	// for imminent rows; the assertion is a substring
	// match because the rest of the markup is the same.
	if !strings.Contains(body, `class="stuck"`) {
		t.Errorf("imminent row missing stuck class: %s", body)
	}
	if !strings.Contains(body, `<span class="chip chip-fail">yes</span>`) {
		t.Errorf("imminent row missing yes chip: %s", body)
	}
}

// TestServeMarkOrphaned_HandlesRealStore covers the full
// happy path: seed an orphan + a fresh row + a
// heartbeated row, POST /admin/mark-orphaned, and assert
// the orphan moved to failed while the others stayed
// running.
func TestServeMarkOrphaned_HandlesRealStore(t *testing.T) {
	h := newTestDashboard(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	orphan := store.Run{
		ID: "boop-a-b-1-aaaaaaa", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", BaseRef: "main", ReviewNumber: 1,
		Reason: "pull_request.opened", InstallationID: 1,
		Status: store.StatusRunning, StartedAt: now.Add(-30 * time.Minute),
	}
	fresh := orphan
	fresh.ID = "boop-a-b-2-bbbbbbb"
	fresh.StartedAt = now.Add(-1 * time.Minute)
	heartbeated := orphan
	heartbeated.ID = "boop-a-b-3-ccccccc"
	heartbeated.StartedAt = now.Add(-30 * time.Minute)
	for _, r := range []store.Run{orphan, fresh, heartbeated} {
		if _, err := h.store.UpsertRun(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
	}
	if err := h.store.TouchRunHeartbeat(ctx, heartbeated.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	req := httptest.NewRequest("POST", "/dashboard/admin/mark-orphaned", nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"marked":1`) {
		t.Errorf("body = %q, want marked=1", body)
	}

	// Orphan row → failed
	got, err := h.store.GetRun(ctx, orphan.ID)
	if err != nil {
		t.Fatalf("get orphan: %v", err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("orphan status = %q, want failed", got.Status)
	}
	if !strings.HasPrefix(got.Error, "orphaned") {
		t.Errorf("orphan error = %q, want orphan prefix", got.Error)
	}
	// Fresh row → still running
	got, err = h.store.GetRun(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("fresh status = %q, want running", got.Status)
	}
	// Heartbeated row → still running
	got, err = h.store.GetRun(ctx, heartbeated.ID)
	if err != nil {
		t.Fatalf("get heartbeated: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("heartbeated status = %q, want running", got.Status)
	}
}

func TestServeMarkOrphaned_RejectsBadGrace(t *testing.T) {
	h := newTestDashboard(t, nil)
	req := httptest.NewRequest("POST", "/dashboard/admin/mark-orphaned?grace=notaduration", nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestServeMarkOrphaned_RejectsGetMethod(t *testing.T) {
	h := newTestDashboard(t, nil)
	req := httptest.NewRequest("GET", "/dashboard/admin/mark-orphaned", nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)
	if rr.Code != http.StatusMethodNotAllowed && rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (only POST is routed)", rr.Code)
	}
}

// TestServeRunDetail_RendersFailedRun covers the full
// run-detail render: a failed run carries an error
// string + a failure_class; the page renders both, plus
// the K8s pod logs (from the Actions.FetchPodLogs
// callback) and the PR link composed from
// owner/repo/PRNumber.
func TestServeRunDetail_RendersFailedRun(t *testing.T) {
	var fetched string
	fetchLogs := func(ctx context.Context, jobName string) (string, error) {
		fetched = jobName
		return `{"stage":"start","msg":"hi"}
{"stage":"opencode","msg":"calling llm"}
{"stage":"opencode","code":1,"msg":"exit non-zero"}
`, nil
	}
	h := newTestDashboardWithLogs(t, nil, fetchLogs)

	ctx := context.Background()
	now := time.Now().UTC()
	run := store.Run{
		ID: "boop-a-b-42-aaaaaaa", Owner: "alice", Repo: "widgets",
		PRNumber: 42, CommitSHA: "aaaaaaa", BaseRef: "main",
		ReviewNumber: 1, Reason: "pull_request.opened", InstallationID: 1,
		Status: store.StatusFailed, StartedAt: now.Add(-2 * time.Minute),
		Error:        "BackoffLimitExceeded: pod crashed before heartbeat",
		FailureClass: "container_error",
	}
	if _, err := h.store.UpsertRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	ended := now.Add(-30 * time.Second)
	dur := int64(90_000)
	if _, err := h.store.UpdateRunStatus(ctx, run.ID, store.StatusFailed, &ended, &dur, run.Error); err != nil {
		t.Fatalf("finalise: %v", err)
	}

	req := httptest.NewRequest("GET", "/dashboard/runs/"+run.ID, nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		"PR #42 on GitHub",     // PR link in toolbar
		"container_error",      // failure class chip
		"BackoffLimitExceeded", // full error string in the Error section
		"opencode",             // a stage name from the seeded K8s logs
		"K8s pod logs",         // the new logs section
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if fetched != run.ID {
		t.Errorf("FetchPodLogs called with %q, want %q", fetched, run.ID)
	}
}

// TestServeRunDetail_RendersLogsUnavailable covers the
// "Job TTL'd" path: the FetchPodLogs callback returns
// ("", nil) and the dashboard renders the empty-state
// hint, not a 500.
func TestServeRunDetail_RendersLogsUnavailable(t *testing.T) {
	h := newTestDashboardWithLogs(t, nil, func(ctx context.Context, jobName string) (string, error) {
		return "", nil
	})
	ctx := context.Background()
	run := seedRunForDashboard(t, h, store.StatusFailed, 0)
	if _, err := h.store.UpsertRun(ctx, run); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("GET", "/dashboard/runs/"+run.ID, nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "TTL'd") {
		t.Errorf("body missing TTL'd hint: %s", rr.Body.String())
	}
}

// TestServeRunDetail_RendersLogsError covers the
// FetchPodLogs-error path: the callback returns an
// error and the dashboard surfaces it as the reason
// logs are unavailable (the operator sees a real error
// rather than a silent gap).
func TestServeRunDetail_RendersLogsError(t *testing.T) {
	h := newTestDashboardWithLogs(t, nil, func(ctx context.Context, jobName string) (string, error) {
		return "", errors.New("kube client timeout")
	})
	ctx := context.Background()
	run := seedRunForDashboard(t, h, store.StatusFailed, 0)
	if _, err := h.store.UpsertRun(ctx, run); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("GET", "/dashboard/runs/"+run.ID, nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "kube client timeout") {
		t.Errorf("body missing error reason: %s", rr.Body.String())
	}
}

// TestServeRunDetail_NoPRLinkForOrphanedRows covers the
// edge case where the run row has no owner/repo/PR (the
// defensive check; MarkOrphanedRuns always carries those
// fields, but a hand-edited row might not). The page must
// still render without the PR link and without a 500.
func TestServeRunDetail_NoPRLinkForOrphanedRows(t *testing.T) {
	h := newTestDashboardWithLogs(t, nil, nil)
	ctx := context.Background()
	run := store.Run{
		ID: "boop-x-y-1-deadbee", Status: store.StatusRunning,
		StartedAt: time.Now().UTC(),
	}
	if _, err := h.store.UpsertRun(ctx, run); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("GET", "/dashboard/runs/"+run.ID, nil)
	rr := httptest.NewRecorder()
	h.route(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "PR #") {
		t.Errorf("body should not contain PR link for owner-less run")
	}
}

// errorSink is a small helper for tests that want to
// assert "no error" but keep error handling explicit.
// Avoids t.Fatalf in tests where the failure mode is
// "returned error" rather than "assertion failed".
func errorSink(err error) error {
	if err != nil {
		return errors.New("got error: " + err.Error())
	}
	return nil
}

// QUB-128: /dashboard/health must require X-Boop-Dashboard-Token.
// The bug was that main.go registered /dashboard/health with a
// bare HandleFunc, bypassing RegisterRoutes' middleware. The
// fix wraps Health in Middleware(...). This test pins the wrap:
// Health through Middleware rejects a missing token with 401.
// The wire shape (handler + middleware) lives in main.go, not
// in this package, so this test asserts the middleware shape
// on its own — and trusts the main.go call site to apply it.
func TestHealth_MiddlewareRejectsMissingToken(t *testing.T) {
	d := newTestDashboard(t, nil)

	// Health directly, no middleware: returns 200 if the
	// store is healthy. This is the pre-fix shape.
	req := httptest.NewRequest("GET", "/dashboard/health", nil)
	rr := httptest.NewRecorder()
	d.Health(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("direct Health: code = %d, want 200", rr.Code)
	}

	// Health through Middleware without a token: 401. This
	// is the post-fix shape — the wrap in main.go applies
	// the same middleware to /dashboard/health as to the
	// rest of /dashboard/*.
	wrapped := d.Middleware(http.HandlerFunc(d.Health))
	req = httptest.NewRequest("GET", "/dashboard/health", nil)
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Health through Middleware (no token): code = %d, want 401", rr.Code)
	}

	// Health through Middleware with the right token: 200.
	wrapped = d.Middleware(http.HandlerFunc(d.Health))
	req = httptest.NewRequest("GET", "/dashboard/health", nil)
	req.Header.Set("X-Boop-Dashboard-Token", "test-dashboard-token")
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Health through Middleware (with token): code = %d, want 200", rr.Code)
	}
}
