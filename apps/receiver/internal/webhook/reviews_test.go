package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCollectReviews(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	active := newJob("boop-qubitquilt-boop-2-b147629", "qubitquilt", "boop", 2, "b147629deadbeef", time.Hour)
	active.Status.Active = 1
	active.Status.StartTime = ptrMeta(now.Add(-6 * time.Minute))

	recent := newJob("boop-qubitquilt-boop-1-1deb259f", "qubitquilt", "boop", 1, "1deb259fdeadbeef", time.Hour)
	recent.Status.Succeeded = 1
	recent.Status.StartTime = ptrMeta(now.Add(-3 * time.Hour))
	recent.Status.CompletionTime = ptrMeta(now.Add(-2 * time.Hour))

	failed := newJob("boop-michaelruelas-family-picnic-platform-18-20cd521a", "michaelruelas", "family-picnic-platform", 18, "20cd521adeadbeef", time.Hour)
	failed.Status.Failed = 1
	failed.Status.StartTime = ptrMeta(now.Add(-58 * time.Minute))
	failed.Status.CompletionTime = ptrMeta(now.Add(-55 * time.Minute))
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metaValue(now.Add(-55 * time.Minute)),
	}}

	// Old failed Job — outside failedWindow — should be dropped.
	oldFailed := newJob("boop-old-old-1-deadbee0", "old", "old", 1, "deadbee00000", time.Hour)
	oldFailed.Status.Failed = 1
	oldFailed.Status.StartTime = ptrMeta(now.Add(-10 * 24 * time.Hour))
	oldFailed.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metaValue(now.Add(-10 * 24 * time.Hour)),
	}}

	// Old successful Job — outside recentWindow — should be dropped.
	oldDone := newJob("boop-old-old-2-deadbee1", "old", "old", 2, "deadbee11111", time.Hour)
	oldDone.Status.Succeeded = 1
	oldDone.Status.StartTime = ptrMeta(now.Add(-72 * time.Hour))
	oldDone.Status.CompletionTime = ptrMeta(now.Add(-72 * time.Hour).Add(5 * time.Minute))

	resp := collectReviews([]batchv1.Job{*oldFailed, *oldDone, *active, *recent, *failed}, now)

	if len(resp.Active) != 1 || resp.Active[0].Name != active.Name {
		t.Errorf("active = %+v, want exactly %s", resp.Active, active.Name)
	}
	if len(resp.Recent) != 1 || resp.Recent[0].Name != recent.Name {
		t.Errorf("recent = %+v, want exactly %s", resp.Recent, recent.Name)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].Name != failed.Name {
		t.Errorf("failed = %+v, want exactly %s", resp.Failed, failed.Name)
	}

	// Sort check: active has only one item, but feeding two jobs should
	// still produce newest-first ordering.
	older := newJob("boop-x-x-3-aaaa", "x", "x", 3, "aaaaaaa", time.Hour)
	older.Status.Active = 1
	older.Status.StartTime = ptrMeta(now.Add(-30 * time.Minute))

	resp = collectReviews([]batchv1.Job{*older, *active}, now)
	if len(resp.Active) != 2 {
		t.Fatalf("active len = %d, want 2", len(resp.Active))
	}
	if resp.Active[0].Name != active.Name || resp.Active[1].Name != older.Name {
		t.Errorf("sort order wrong: %s then %s", resp.Active[0].Name, resp.Active[1].Name)
	}
}

func TestReviewFromJob_FullMetadata(t *testing.T) {
	now := time.Now()
	job := newJob("boop-qubitquilt-boop-2-b147629", "qubitquilt", "boop", 2, "b147629deadbeef", time.Hour)
	job.Status.Active = 1
	job.Status.StartTime = ptrMeta(now.Add(-time.Minute))
	job.Status.CompletionTime = ptrMeta(now)

	r := reviewFromJob(job)

	if r.Name != "boop-qubitquilt-boop-2-b147629" {
		t.Errorf("Name = %q", r.Name)
	}
	if r.Owner != "qubitquilt" {
		t.Errorf("Owner = %q", r.Owner)
	}
	if r.Repo != "boop" {
		t.Errorf("Repo = %q", r.Repo)
	}
	if r.PR != 2 {
		t.Errorf("PR = %d", r.PR)
	}
	if r.Commit != "b147629deadbeef" {
		t.Errorf("Commit = %q", r.Commit)
	}
	if r.BaseRef != "main" {
		t.Errorf("BaseRef = %q", r.BaseRef)
	}
	if r.Status != "Running" {
		t.Errorf("Status = %q, want Running", r.Status)
	}
	if r.Active != 1 || r.Succeeded != 0 || r.Failed != 0 {
		t.Errorf("counts wrong: %+v", r)
	}
	if r.StartTime == "" || r.CompletionTime == "" {
		t.Errorf("timestamps missing: %+v", r)
	}
}

func TestReviewFromJob_MissingLabels(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy",
			Namespace: "dev-tools",
		},
	}
	r := reviewFromJob(job)
	if r.Owner != "" || r.Repo != "" || r.PR != 0 || r.Commit != "" || r.BaseRef != "" {
		t.Errorf("missing-label review should be sparse: %+v", r)
	}
	if r.Status != "Pending" {
		t.Errorf("Status = %q, want Pending", r.Status)
	}
}

func TestHumanStatus(t *testing.T) {
	cases := []struct {
		name string
		job  *batchv1.Job
		want string
	}{
		{"empty", &batchv1.Job{}, "Pending"},
		{"active", &batchv1.Job{Status: batchv1.JobStatus{Active: 1}}, "Running"},
		{"succeeded", &batchv1.Job{Status: batchv1.JobStatus{Succeeded: 1}}, "Complete"},
		{"failed-count", &batchv1.Job{Status: batchv1.JobStatus{Failed: 1}}, "Failed"},
		{"failed-cond", &batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}},
		}}, "Failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanStatus(tc.job); got != tc.want {
				t.Errorf("humanStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListReviews_Handler(t *testing.T) {
	// Use time.Now() so the test stays in the recent/failed
	// windows regardless of when it runs. The previous version
	// anchored `now` to 2026-07-29, which was inside the 24h
	// window when the test was first written but aged out a
	// few days later, leaving the recent bucket empty.
	now := time.Now()

	boopActive := newJob("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaadeadbeef", time.Hour)
	boopActive.Status.Active = 1
	boopActive.Status.StartTime = ptrMeta(now.Add(-time.Minute))

	boopDone := newJob("boop-a-b-2-bbbbbbb", "a", "b", 2, "bbbbbbbdeadbeef", time.Hour)
	boopDone.Status.Succeeded = 1
	boopDone.Status.StartTime = ptrMeta(now.Add(-30 * time.Minute))
	boopDone.Status.CompletionTime = ptrMeta(now.Add(-25 * time.Minute))

	// Non-boop Job — must be excluded by the label selector. The fake
	// clientset still returns it if it is in the tracker, so the handler
	// relies on LabelSelector to filter. To prove the selector works, we
	// preload it and confirm the response has only two items.
	other := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-boop",
			Namespace: "dev-tools",
			Labels:    map[string]string{"app": "other"},
		},
	}

	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add batchv1 to scheme: %v", err)
	}
	client := fake.NewSimpleClientset(boopActive, boopDone, other)

	h := &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
		},
		kube:   client,
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	req := httptest.NewRequest("GET", "/api/reviews", nil).WithContext(context.Background())
	rr := httptest.NewRecorder()
	h.ListReviews(rr, req.WithContext(context.Background()))

	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}

	var resp ReviewsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Active) != 1 || resp.Active[0].Name != boopActive.Name {
		t.Errorf("active = %+v", resp.Active)
	}
	if len(resp.Recent) != 1 || resp.Recent[0].Name != boopDone.Name {
		t.Errorf("recent = %+v", resp.Recent)
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed should be empty: %+v", resp.Failed)
	}
}

func TestListReviews_KubeError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add batchv1 to scheme: %v", err)
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "jobs", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated kube outage")
	})

	h := &Handler{
		cfg:    Config{TargetNamespace: "dev-tools"},
		kube:   client,
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	req := httptest.NewRequest("GET", "/api/reviews", nil)
	rr := httptest.NewRecorder()
	h.ListReviews(rr, req)
	if rr.Code != 503 {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// newJob builds a Job with the labels/annotations the receiver template
// would set, so tests can exercise reviewFromJob and the handler end to
// end without re-implementing the template rendering.
func newJob(name, owner, repo string, number int, sha string, _ time.Duration) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "dev-tools",
			Labels: map[string]string{
				"app":       "boop",
				"pr-number": itoa(number),
				"sha":       sha,
			},
			Annotations: map[string]string{
				"boop/owner":    owner,
				"boop/repo":     repo,
				"boop/number":   itoa(number),
				"boop/sha":      sha,
				"boop/base-ref": "main",
			},
		},
	}
}

func ptrMeta(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

func metaValue(t time.Time) metav1.Time {
	return metav1.NewTime(t)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
