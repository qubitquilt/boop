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
