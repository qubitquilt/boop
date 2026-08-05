package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
)

// recordingRoundTripper is a minimal http.RoundTripper that routes
// every GitHub call to a function the test supplies and records the
// (method, path) of every call in the order it arrives. It is
// swapped into Manager via Manager.SetHTTPClient so the production
// Manager + Client both route through it without standing up a real
// HTTP server.
type recordingRoundTripper struct {
	mu      sync.Mutex
	calls   []recordedCall
	handler func(req *http.Request) (*http.Response, error)
}

type recordedCall struct {
	Method string
	Path   string
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{Method: req.Method, Path: req.URL.Path})
	r.mu.Unlock()
	return r.handler(req)
}

func (r *recordingRoundTripper) snapshot() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// jsonResp builds an http.Response with the given body and status.
func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// newRecordingHandler returns an http.RoundTripper that handles the
// routes the receiver needs: app-info, app-installations token, PR
// fetch, prior-review comments list, and reaction add. The PR /
// sender fields are baked into the closure so the test can drive
// the handler with the right fixtures.
type recordingOpts struct {
	prOwner      string
	prRepo       string
	prNumber     int
	prHeadSHA    string
	prBaseRef    string
	senderLogin  string // for issue_comment AppBotLogin / sender matching
	priorReviews int
	commentID    int64
}

func newRecordingHandler(opts recordingOpts) *recordingRoundTripper {
	r := &recordingRoundTripper{}
	r.handler = func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/app":
			return jsonResp(200, `{"slug":"booppr"}`), nil
		case req.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", 12345):
			return jsonResp(200, `{"token":"ghs_test","expires_at":"2030-01-01T00:00:00Z"}`), nil
		case req.URL.Path == fmt.Sprintf("/repos/%s/%s/pulls/%d", opts.prOwner, opts.prRepo, opts.prNumber):
			return jsonResp(200, fmt.Sprintf(`{
				"number": %d,
				"head": {"sha": %q},
				"base": {"ref": %q}
			}`, opts.prNumber, opts.prHeadSHA, opts.prBaseRef)), nil
		case req.URL.Path == fmt.Sprintf("/repos/%s/%s/issues/%d/comments", opts.prOwner, opts.prRepo, opts.prNumber):
			return jsonResp(200, `[]`), nil
		case strings.HasPrefix(req.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/comments/", opts.prOwner, opts.prRepo)) && strings.HasSuffix(req.URL.Path, "/reactions"):
			return jsonResp(200, `{"id":1,"content":"eyes"}`), nil
		}
		// Default: 404 so unexpected calls surface.
		return jsonResp(404, `{"message":"not found: `+req.URL.Path+`"}`), nil
	}
	return r
}

func newRecordingHandlerForTest(t *testing.T, opts recordingOpts) (*recordingRoundTripper, *boopgithub.Manager) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	rt := newRecordingHandler(opts)
	mgr := boopgithub.NewManager(boopgithub.AppConfig{AppID: 1, PrivateKey: priv})
	mgr.SetBaseHTTPForTest(&http.Client{Transport: rt})
	// The recordingRoundTripper routes on URL path only; the
	// host is irrelevant. Point AppBotLogin at a benign
	// localhost so the /app hit lands on our handler.
	// Save the package-level appInfoURL so other tests (and
	// the background poller) keep their original endpoint
	// after this test returns.
	orig := boopgithub.AppInfoURLForTest()
	boopgithub.SetAppInfoURLForTest("http://test/app")
	t.Cleanup(func() { boopgithub.SetAppInfoURLForTest(orig) })
	return rt, mgr
}

func newHandlerWithKubeAndGH(t *testing.T, kube *fake.Clientset, mgr *boopgithub.Manager) *Handler {
	t.Helper()
	return &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			JobImage:        "ghcr.io/qubitquilt/boop-runner:dev",
			BotLogin:        "booppr[bot]",
		},
		kube:    kube,
		ghClient: mgr,
		logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		dedup:   newDeliveryDedup(4096),
		limiter: nil,
	}
}

// --- QUB-99: pull_request path ----------------------------------------

func TestHandlePullRequest_NoStatusCommentFromReceiver_PinsQUB99(t *testing.T) {
	const (
		owner  = "qubitquilt"
		repo   = "boop"
		number = 99
		sha    = "7e895631f15f6ba1a542b5cbf68d7dc8d887de82"
	)
	rt, mgr := newRecordingHandlerForTest(t, recordingOpts{
		prOwner: owner, prRepo: repo, prNumber: number, prHeadSHA: sha, prBaseRef: "main",
	})
	kube := fake.NewSimpleClientset()
	h := newHandlerWithKubeAndGH(t, kube, mgr)

	body := pullRequestBody(t, owner, repo, number, sha, "main", "opened", nil)
	w := httptest.NewRecorder()
	h.handlePullRequest(context.Background(), w, "delivery-1", 12345, []byte(body))

	if w.Code != 202 {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	// Exactly one K8s Job.
	jobs, err := kube.BatchV1().Jobs("dev-tools").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 Job, got %d", len(jobs.Items))
	}
	// The Job's BOOP_STATUS_COMMENT_ID is 0 (the runner creates it).
	if got := jobEnvValue(jobs.Items[0], "BOOP_STATUS_COMMENT_ID"); got != "0" {
		t.Errorf("BOOP_STATUS_COMMENT_ID = %q, want 0 (runner creates initial)", got)
	}
	// QUB-99 invariant: the receiver MUST NOT call PostIssueComment
	// on the pull_request path. The runner is the only one that
	// posts a status comment, so any POST to /repos/.../comments
	// (without /reactions) is an orphan.
	for _, c := range rt.snapshot() {
		if c.Method == "POST" && strings.Contains(c.Path, "/issues/"+itoaForTest(number)+"/comments") && !strings.Contains(c.Path, "/reactions") {
			t.Errorf("receiver called POST %q; QUB-99: only the runner posts a status comment", c.Path)
		}
	}
}

// --- QUB-99: issue_comment path ---------------------------------------

func TestHandleIssueComment_ReactionAfterCreateJob_PinsQUB99(t *testing.T) {
	const (
		owner  = "qubitquilt"
		repo   = "boop"
		number = 100
		sha    = "20cd521abcdef0123456789abcdef0123456789"
		by     = "alice"
	)
	rt, mgr := newRecordingHandlerForTest(t, recordingOpts{
		prOwner: owner, prRepo: repo, prNumber: number, prHeadSHA: sha, prBaseRef: "main",
		senderLogin: by, commentID: 5153677875,
	})
	kube := fake.NewSimpleClientset()
	h := newHandlerWithKubeAndGH(t, kube, mgr)

	body := issueCommentBody(t, owner, repo, number, by, "@BoopPr review")
	w := httptest.NewRecorder()
	h.handleIssueComment(context.Background(), w, "delivery-1", 12345, []byte(body))

	if w.Code != 202 {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	jobs, err := kube.BatchV1().Jobs("dev-tools").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 Job, got %d", len(jobs.Items))
	}
	if got := jobEnvValue(jobs.Items[0], "BOOP_STATUS_COMMENT_ID"); got != "0" {
		t.Errorf("BOOP_STATUS_COMMENT_ID = %q, want 0 (runner creates initial)", got)
	}
	if got := jobEnvValue(jobs.Items[0], "BOOP_SENDER_LOGIN"); got != by {
		t.Errorf("BOOP_SENDER_LOGIN = %q, want %q", got, by)
	}

	// QUB-99 invariant: PostIssueComment is NEVER called on the
	// happy path; AddCommentReaction is called exactly once.
	reactionCalls := 0
	statusPosts := 0
	for _, c := range rt.snapshot() {
		if c.Method == "POST" && strings.Contains(c.Path, "/reactions") {
			reactionCalls++
		}
		if c.Method == "POST" && strings.Contains(c.Path, "/issues/"+itoaForTest(number)+"/comments") && !strings.Contains(c.Path, "/reactions") {
			statusPosts++
		}
	}
	if reactionCalls != 1 {
		t.Errorf("AddCommentReaction called %d times, want 1", reactionCalls)
	}
	if statusPosts != 0 {
		t.Errorf("PostIssueComment called %d times; QUB-99 forbids it on the happy path", statusPosts)
	}
}

// --- QUB-99: failure path ---------------------------------------------

func TestHandleIssueComment_NoReactionWhenCreateJobFails_PinsQUB99(t *testing.T) {
	const (
		owner  = "qubitquilt"
		repo   = "boop"
		number = 101
		sha    = "30cd521abcdef0123456789abcdef0123456789"
		by     = "bob"
	)
	rt, mgr := newRecordingHandlerForTest(t, recordingOpts{
		prOwner: owner, prRepo: repo, prNumber: number, prHeadSHA: sha, prBaseRef: "main",
		senderLogin: by, commentID: 5153677875,
	})
	kube := fake.NewSimpleClientset()
	// Force createJob to fail with a 500-shape error.
	kube.PrependReactor("create", "jobs", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated kube outage")
	})
	h := newHandlerWithKubeAndGH(t, kube, mgr)

	body := issueCommentBody(t, owner, repo, number, by, "@BoopPr review")
	w := httptest.NewRecorder()
	h.handleIssueComment(context.Background(), w, "delivery-1", 12345, []byte(body))

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	// The 👀 reaction must NOT have been called: without a Job
	// there is no runner to follow up.
	for _, c := range rt.snapshot() {
		if c.Method == "POST" && strings.Contains(c.Path, "/reactions") {
			t.Errorf("AddCommentReaction at %q despite createJob failure; orphan reaction", c.Path)
		}
	}
}

// --- QUB-99: redelivery after a process death -------------------------
//
// Simulates a GitHub 5xx-driven redelivery after the receiver
// process was killed between createJob and the reaction. The
// dedup table (delivery-dedup) is the only thing that protects
// the second delivery from double-submitting. With dedup ON the
// second delivery is a no-op (no second K8s Job, no second
// reaction, no second status comment); with dedup OFF the
// second delivery would post a SECOND status comment, which is
// the orphan this issue is meant to delete.
//
// We do not exercise the postStatus path here because the
// receiver no longer posts a status comment at all (the runner
// does). The redelivery simulation is therefore focused on the
// receiver's contribution: dedup + K8s Job dedup (via
// claimJobSlot) prevent double-runs. A redelivery that lands
// after the original Job is "active" returns "duplicate" and
// makes no GitHub calls.
//
// The redelivery test goes through HandleWebhook (not
// handleIssueComment directly) so the dedup table — which lives
// in HandleWebhook — actually short-circuits the second
// delivery. To bypass the HMAC verification we set WebhookSecret
// to a known value and sign the body with that secret.
func TestHandleIssueComment_RedeliveryAfterProcessDeath_NoOrphanComments_PinsQUB99(t *testing.T) {
	const (
		owner  = "qubitquilt"
		repo   = "boop"
		number = 102
		sha    = "40cd521abcdef0123456789abcdef0123456789"
		by     = "carol"
		secret = "test-webhook-secret"
	)
	rt, mgr := newRecordingHandlerForTest(t, recordingOpts{
		prOwner: owner, prRepo: repo, prNumber: number, prHeadSHA: sha, prBaseRef: "main",
		senderLogin: by, commentID: 5153677875,
	})
	kube := fake.NewSimpleClientset()
	h := newHandlerWithKubeAndGH(t, kube, mgr)
	h.cfg.WebhookSecret = secret

	body := issueCommentBody(t, owner, repo, number, by, "@BoopPr review")
	const delivery = "delivery-redeliver"

	// First delivery — happy path.
	w1 := httptest.NewRecorder()
	r1 := signedWebhookRequest(t, "issue_comment", body, delivery, secret, "12345")
	h.HandleWebhook(w1, r1)
	if w1.Code != 202 {
		t.Fatalf("first delivery: status = %d, want 202; body = %s", w1.Code, w1.Body.String())
	}
	firstCallCount := len(rt.snapshot())
	if jobCount(t, kube) != 1 {
		t.Fatalf("first delivery: jobs = %d, want 1", jobCount(t, kube))
	}
	if firstCallCount == 0 {
		t.Fatal("first delivery: no calls recorded; fixture broken")
	}

	// Second delivery — same X-GitHub-Delivery header (a
	// redelivery, not a fresh event). The dedup table inside
	// HandleWebhook short-circuits the second delivery BEFORE
	// the per-event handler can do any work, so no extra
	// GitHub calls land on the recording server.
	w2 := httptest.NewRecorder()
	r2 := signedWebhookRequest(t, "issue_comment", body, delivery, secret, "12345")
	h.HandleWebhook(w2, r2)
	if w2.Code != 202 {
		t.Fatalf("redelivery: status = %d, want 202; body = %s", w2.Code, w2.Body.String())
	}
	if got := jobCount(t, kube); got != 1 {
		t.Errorf("redelivery: jobs = %d, want 1 (no second Job)", got)
	}
	if got := len(rt.snapshot()); got != firstCallCount {
		t.Errorf("redelivery: extra API calls recorded = %d, want 0", got-firstCallCount)
	}
}

// signedWebhookRequest builds a real *http.Request that the
// receiver's HMAC verification will accept. Reuses the body for
// both the request body and the signature, so HandleWebhook
// processes the same payload the test would have passed
// directly to handleIssueComment.
func signedWebhookRequest(t *testing.T, event, body, delivery, secret, installID string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Installation-ID", installID)
	req.Header.Set("X-Hub-Signature-256", sig)
	return req
}

// --- helpers ------------------------------------------------------------

func jobCount(t *testing.T, kube *fake.Clientset) int {
	t.Helper()
	jobs, err := kube.BatchV1().Jobs("dev-tools").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return len(jobs.Items)
}

func pullRequestBody(t *testing.T, owner, repo string, number int, sha, baseRef, action string, labels []string) string {
	t.Helper()
	labelJSON := "["
	for i, l := range labels {
		if i > 0 {
			labelJSON += ","
		}
		labelJSON += fmt.Sprintf(`{"name":%q}`, l)
	}
	labelJSON += "]"
	// Note: GitHub's pull_request event puts `number` at the TOP
	// level of the payload, not inside `pull_request`. The
	// receiver's parsePullRequest reads it via
	// `pr.GetNumber()` against the *github.PullRequestEvent
	// struct, which mirrors that schema.
	return fmt.Sprintf(`{
		"action": %q,
		"number": %d,
		"installation": {"id": 12345},
		"pull_request": {
			"number": %d,
			"head": {"sha": %q},
			"base": {"ref": %q},
			"labels": %s
		},
		"repository": {
			"owner": {"login": %q},
			"name": %q
		}
	}`, action, number, number, sha, baseRef, labelJSON, owner, repo)
}

func issueCommentBody(t *testing.T, owner, repo string, number int, sender, body string) string {
	t.Helper()
	return fmt.Sprintf(`{
		"action": "created",
		"installation": {"id": 12345},
		"issue": {
			"number": %d,
			"pull_request": {"url": "https://api.github.com/.../pulls/%d"}
		},
		"comment": {
			"id": 5153677875,
			"body": %q
		},
		"sender": {"login": %q},
		"repository": {
			"owner": {"login": %q},
			"name": %q
		}
	}`, number, number, body, sender, owner, repo)
}

func jobEnvValue(job batchv1.Job, name string) string {
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}

func itoaForTest(n int) string {
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

// json import is referenced via Unmarshal in some fixtures; keep the
// import alive so the file builds when fixtures are added.
var _ = json.Marshal
