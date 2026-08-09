package webhook

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestHandlerWithStoreAndKube builds a Handler with the
// same shape main.go builds: a fake kube client, a real
// (file-backed) store. Used by the rerun tests so the K8s
// jobbuilder half can be exercised end-to-end.
func newTestHandlerWithStoreAndKube(t *testing.T) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boop.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{
			"JOB_IMAGE": "ghcr.io/qubitquilt/boop-runner@sha256:test",
		},
	}
	client := fake.NewSimpleClientset(cm)
	return &Handler{
		cfg: Config{
			TargetNamespace:      "dev-tools",
			RunnerToken:          "test-runner-token",
			OpenRouterSDKDefault: "1",
			OpenRouterModel:      "test-model",
		},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		store:  s,
		kube:   client,
	}
}

func seedTestRun(t *testing.T, h *Handler, status store.RunStatus) store.Run {
	t.Helper()
	ctx := context.Background()
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
	if _, err := h.store.UpsertRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run
}

// TestCreateRerunJob_PersistsLineage covers the load-bearing
// QUB-110 invariants: the new run row has parent_run_id set,
// superseded_by_id is backfilled on the prior, and the new
// run's id follows the {original}-r1 convention.
func TestCreateRerunJob_PersistsLineage(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	prior := seedTestRun(t, h, store.StatusFailed)

	ctx := context.Background()
	newID, err := h.CreateRerunJob(ctx, prior, "exception dock requeue")
	if err != nil {
		t.Fatalf("CreateRerunJob: %v", err)
	}
	if newID != "boop-a-b-1-aaaaaaa-r1" {
		t.Errorf("new id = %q, want boop-a-b-1-aaaaaaa-r1", newID)
	}
	updated, err := h.store.GetRun(ctx, prior.ID)
	if err != nil {
		t.Fatalf("get prior: %v", err)
	}
	if updated.SupersededByID != newID {
		t.Errorf("prior.superseded_by_id = %q, want %q", updated.SupersededByID, newID)
	}
	child, err := h.store.GetRun(ctx, newID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if child.ParentRunID != prior.ID {
		t.Errorf("child.parent_run_id = %q, want %q", child.ParentRunID, prior.ID)
	}
	if child.Status != store.StatusPending {
		t.Errorf("child.status = %q, want pending", child.Status)
	}
	if child.ReviewNumber != prior.ReviewNumber+1 {
		t.Errorf("child.review_number = %d, want %d", child.ReviewNumber, prior.ReviewNumber+1)
	}
	if !strings.HasPrefix(child.Reason, "rerun: ") {
		t.Errorf("child.reason = %q, want rerun: prefix", child.Reason)
	}
}

// TestCreateRerunJob_CreatesK8sJob pins the K8s half
// (Phase 4). The fake clientset records every Create; the
// test asserts the Job exists with the new name and that
// the parent-run id lands in BOTH the annotation and the
// env var (two surfaces because future readers — operator
// via kubectl describe, runner via process env — may pick
// different ones).
func TestCreateRerunJob_CreatesK8sJob(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	prior := seedTestRun(t, h, store.StatusFailed)

	newID, err := h.CreateRerunJob(context.Background(), prior, "test rerun")
	if err != nil {
		t.Fatalf("CreateRerunJob: %v", err)
	}
	job, err := h.kube.BatchV1().Jobs("dev-tools").Get(context.Background(), newID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Name != newID {
		t.Errorf("job.Name = %q, want %q", job.Name, newID)
	}
	ann := job.Spec.Template.ObjectMeta.Annotations
	if ann["boop/parent-run-id"] != prior.ID {
		t.Errorf("annotation parent = %q, want %q", ann["boop/parent-run-id"], prior.ID)
	}
	var found bool
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "BOOP_PARENT_RUN_ID" {
			if env.Value != prior.ID {
				t.Errorf("BOOP_PARENT_RUN_ID = %q, want %q", env.Value, prior.ID)
			}
			found = true
		}
	}
	if !found {
		t.Error("BOOP_PARENT_RUN_ID env var missing from Job")
	}
	// QUB-126: OPENROUTER_MODEL must be forwarded on
	// re-runs. The main submit path (handler.go) sets it
	// from h.cfg.OpenRouterModel; the rerun path missed
	// this in PR #135 and every dashboard requeue has
	// landed the runner with OPENROUTER_MODEL="" since,
	// tripping the runner's `model is required` guard.
	// A unit test that fails on the current code keeps
	// the regression from coming back.
	var modelEnv *corev1.EnvVar
	for i, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "OPENROUTER_MODEL" {
			modelEnv = &job.Spec.Template.Spec.Containers[0].Env[i]
		}
	}
	if modelEnv == nil {
		t.Error("OPENROUTER_MODEL env var missing from Job")
	} else if modelEnv.Value != "test-model" {
		t.Errorf("OPENROUTER_MODEL = %q, want %q (QUB-126: rerun path must forward the receiver's OPENROUTER_MODEL)", modelEnv.Value, "test-model")
	}
}

// TestCreateRerunJob_NumberedAcrossCalls asserts the
// -r{N} counter increments across multiple re-runs of
// the same prior run. A -r3 must actually be -r3 even
// if the prior was already superseded once.
func TestCreateRerunJob_NumberedAcrossCalls(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	prior := seedTestRun(t, h, store.StatusFailed)
	ctx := context.Background()

	r1, err := h.CreateRerunJob(ctx, prior, "first")
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	if r1 != "boop-a-b-1-aaaaaaa-r1" {
		t.Errorf("r1 = %q, want boop-a-b-1-aaaaaaa-r1", r1)
	}
	r2, err := h.CreateRerunJob(ctx, prior, "second")
	if err != nil {
		t.Fatalf("r2: %v", err)
	}
	if r2 != "boop-a-b-1-aaaaaaa-r2" {
		t.Errorf("r2 = %q, want ...-r2", r2)
	}
	r3, err := h.CreateRerunJob(ctx, prior, "third")
	if err != nil {
		t.Fatalf("r3: %v", err)
	}
	if r3 != "boop-a-b-1-aaaaaaa-r3" {
		t.Errorf("r3 = %q, want ...-r3", r3)
	}
}

// TestCreateRerunJob_NonTerminalRejected guards against
// the failure mode where an operator's stale tab
// schedules a re-run against a still-running prior
// Job. We reject at the boundary instead of letting
// the count flip.
func TestCreateRerunJob_NonTerminalRejected(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	prior := seedTestRun(t, h, store.StatusRunning)

	_, err := h.CreateRerunJob(context.Background(), prior, "stale tab")
	if err == nil {
		t.Fatal("expected error for non-terminal prior")
	}
}

// TestRerun_RejectsEmptyConfirm pins the CSRF defense.
// A missing or false confirm returns 400 without
// touching the K8s client or the store.
func TestRerun_RejectsEmptyConfirm(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	seedTestRun(t, h, store.StatusFailed)

	rr := httptest.NewRecorder()
	// QUB-127: auth is checked before body validation.
	// A request with no token gets 401, not 400. The
	// existing test was written before the auth gate
	// landed; the new behavior is what we want — an
	// attacker probing the public surface sees a stable
	// 401 regardless of payload shape, not a 400 that
	// confirms the path is reachable.
	req := httptest.NewRequest("POST", "/api/runs/boop-a-b-1-aaaaaaa/rerun", strings.NewReader(`{"reason":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "boop-a-b-1-aaaaaaa")
	h.Rerun(rr, req)
	if rr.Code != 401 {
		t.Errorf("status = %d, want 401 (no token)", rr.Code)
	}
}

// QUB-127: with a valid token, the body-validation path
// returns 400 for missing confirm. This is the original
// CSRF defense the empty-confirm test was supposed to
// pin; auth now gates it.
func TestRerun_RejectsEmptyConfirm_WithToken(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	seedTestRun(t, h, store.StatusFailed)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/runs/boop-a-b-1-aaaaaaa/rerun", strings.NewReader(`{"reason":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BOOP-Runner-Token", "test-runner-token")
	req.SetPathValue("id", "boop-a-b-1-aaaaaaa")
	h.Rerun(rr, req)
	if rr.Code != 400 {
		t.Errorf("status = %d, want 400 (confirm required)", rr.Code)
	}
}

// QUB-127: the rerun handler must check X-BOOP-Runner-Token
// before any work. A POST without the token returns 401
// without touching the K8s client or the store. The QUB-115
// public surface would otherwise accept unauthenticated
// requeues and mint Jobs with the runner image's secrets.
func TestRerun_RejectsMissingToken(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	seedTestRun(t, h, store.StatusFailed)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/runs/boop-a-b-1-aaaaaaa/rerun", strings.NewReader(`{"reason":"x","confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "boop-a-b-1-aaaaaaa")
	h.Rerun(rr, req)
	if rr.Code != 401 {
		t.Errorf("status = %d, want 401 (no token)", rr.Code)
	}
}

func TestRerun_RejectsWrongToken(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	seedTestRun(t, h, store.StatusFailed)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/runs/boop-a-b-1-aaaaaaa/rerun", strings.NewReader(`{"reason":"x","confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BOOP-Runner-Token", "wrong")
	req.SetPathValue("id", "boop-a-b-1-aaaaaaa")
	h.Rerun(rr, req)
	if rr.Code != 401 {
		t.Errorf("status = %d, want 401 (wrong token)", rr.Code)
	}
}

// QUB-127 happy path: with the right token + confirm + reason,
// the rerun returns 202. Mirrors the production CLI path.
func TestRerun_HappyPath(t *testing.T) {
	h := newTestHandlerWithStoreAndKube(t)
	seedTestRun(t, h, store.StatusFailed)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/runs/boop-a-b-1-aaaaaaa/rerun", strings.NewReader(`{"reason":"exception dock requeue","confirm":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BOOP-Runner-Token", "test-runner-token")
	req.SetPathValue("id", "boop-a-b-1-aaaaaaa")
	h.Rerun(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHmacFailActor covers the actor derivation for the
// audit ledger. The two interesting cases: an X-Forwarded-For
// chain (production behind a proxy) and a direct connection
// (smoke test). The order matters because we take the
// leftmost address — a misconfigured proxy appending
// client IPs instead of prepending would silently lose the
// true peer.
func TestHmacFailActor(t *testing.T) {
	cases := []struct {
		name       string
		xff        string
		remoteAddr string
		want       string
	}{
		{"xff single", "1.2.3.4", "127.0.0.1:0", "unauthenticated:1.2.3.4"},
		{"xff chain takes leftmost", "1.2.3.4, 10.0.0.1", "127.0.0.1:0", "unauthenticated:1.2.3.4"},
		{"xff trimmed", "  1.2.3.4  , 10.0.0.1", "127.0.0.1:0", "unauthenticated:1.2.3.4"},
		{"no xff uses remoteaddr", "", "10.0.0.5:54321", "unauthenticated:10.0.0.5"},
		{"no xff no remoteaddr", "", "", "unauthenticated:unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhook", nil)
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			r.RemoteAddr = c.remoteAddr
			got := hmacFailActor(r)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
