package webhook

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

func TestDeriveTerminalState_FailedCondition(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	job := &batchv1.Job{}
	job.Status.Failed = 2
	job.Status.CompletionTime = ptrMeta(now)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Reason:  "BackoffLimitExceeded",
		Message: "Job has reached the specified backoff limit",
	}}

	status, errMsg, endedAt := deriveTerminalState(job)
	if status != store.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if errMsg != "BackoffLimitExceeded: Job has reached the specified backoff limit" {
		t.Errorf("errMsg = %q", errMsg)
	}
	if endedAt == nil || !endedAt.Equal(now) {
		t.Errorf("endedAt = %v, want %v", endedAt, now)
	}
}

func TestDeriveTerminalState_CompleteCondition(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	job := &batchv1.Job{}
	job.Status.Succeeded = 1
	job.Status.CompletionTime = ptrMeta(now)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:   batchv1.JobComplete,
		Status: corev1.ConditionTrue,
	}}

	status, errMsg, endedAt := deriveTerminalState(job)
	if status != store.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty", errMsg)
	}
	if endedAt == nil {
		t.Error("endedAt nil")
	}
}

func TestDeriveTerminalState_NoConditionsFallback(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	job := &batchv1.Job{}
	job.Status.Failed = 3
	job.Status.CompletionTime = ptrMeta(now)

	status, errMsg, _ := deriveTerminalState(job)
	if status != store.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if errMsg != "reconciled: 3 failed pods" {
		t.Errorf("errMsg = %q, want fallback", errMsg)
	}
}

func TestDeriveTerminalState_NotTerminal(t *testing.T) {
	job := &batchv1.Job{}
	job.Status.Active = 1
	status, _, _ := deriveTerminalState(job)
	if status != "" {
		t.Errorf("status = %q, want empty", status)
	}
}

func TestReconcileJobsOnce_OrphanedRunBecomesFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	st := openTestStore(t)
	runID := "boop-a-b-1-aaaaaaa"
	if _, err := st.UpsertRun(ctx, store.Run{
		ID:        runID,
		Owner:     "a",
		Repo:      "b",
		PRNumber:  1,
		CommitSHA: "aaaaaaa",
		BaseRef:   "main",
		Status:    store.StatusRunning,
		StartedAt: now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	job := newJob(runID, "a", "b", 1, "aaaaaaa", time.Hour)
	job.Status.Failed = 1
	job.Status.StartTime = ptrMeta(now.Add(-2 * time.Minute))
	job.Status.CompletionTime = ptrMeta(now.Add(-30 * time.Second))
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Reason:  "BackoffLimitExceeded",
		Message: "exceeded",
	}}

	h := newReconcileTestHandler(t, st, job)
	h.reconcileJobsOnce(ctx)

	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Error != "BackoffLimitExceeded: exceeded" {
		t.Errorf("error = %q, want %q", got.Error, "BackoffLimitExceeded: exceeded")
	}
}

func TestReconcileJobsOnce_LeavesRunnerFinalisedRowsAlone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	st := openTestStore(t)
	runID := "boop-a-b-1-aaaaaaa"
	if _, err := st.UpsertRun(ctx, store.Run{
		ID:        runID,
		Owner:     "a",
		Repo:      "b",
		PRNumber:  1,
		CommitSHA: "aaaaaaa",
		BaseRef:   "main",
		Status:    store.StatusRunning,
		StartedAt: now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	ended := now.Add(-30 * time.Second)
	dur := int64(90_000)
	if _, err := st.UpdateRunStatus(ctx, runID, store.StatusSucceeded, &ended, &dur, "runner said done"); err != nil {
		t.Fatalf("runner finalise: %v", err)
	}

	job := newJob(runID, "a", "b", 1, "aaaaaaa", time.Hour)
	job.Status.Succeeded = 1
	job.Status.CompletionTime = ptrMeta(now.Add(-30 * time.Second))
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:   batchv1.JobComplete,
		Status: corev1.ConditionTrue,
	}}

	h := newReconcileTestHandler(t, st, job)
	h.reconcileJobsOnce(ctx)

	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != store.StatusSucceeded {
		t.Errorf("status = %q, want succeeded (runner-finalised)", got.Status)
	}
	if got.Error != "runner said done" {
		t.Errorf("error overwritten: %q", got.Error)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boop.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newReconcileTestHandler(t *testing.T, st *store.Store, jobs ...*batchv1.Job) *Handler {
	t.Helper()
	objs := make([]runtime.Object, len(jobs))
	for i, j := range jobs {
		objs[i] = j
	}
	kube := fake.NewSimpleClientset(objs...)
	return &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
		},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		store:  st,
		kube:   kube,
	}
}