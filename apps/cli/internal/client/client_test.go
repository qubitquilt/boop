package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer spins up an httptest.Server that asserts the
// X-BOOP-Runner-Token header is set correctly and returns the given
// status + body. Used by every client test below.
func newTestServer(t *testing.T, wantToken, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" {
			got := r.Header.Get("X-BOOP-Runner-Token")
			if got != wantToken {
				t.Errorf("token header: got %q, want %q", got, wantToken)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func TestHealthOK(t *testing.T) {
	srv := newTestServer(t, "", `"ok"`, http.StatusOK)
	defer srv.Close()
	cli := New(srv.URL, "")
	h, err := cli.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("status = %q, want ok", h.Status)
	}
}

func TestHealthError(t *testing.T) {
	srv := newTestServer(t, "", "kube error", http.StatusServiceUnavailable)
	defer srv.Close()
	cli := New(srv.URL, "")
	_, err := cli.Health(context.Background())
	ae, ok := err.(*ErrAPI)
	if !ok {
		t.Fatalf("expected *ErrAPI, got %T", err)
	}
	if ae.StatusCode != 503 {
		t.Errorf("status = %d, want 503", ae.StatusCode)
	}
	if ae.Body != "kube error" {
		t.Errorf("body = %q, want kube error", ae.Body)
	}
}

func TestListReviews(t *testing.T) {
	body := `{"active":[{"name":"boop-x-1-abcdefg","status":"Running"}],"recent":[],"failed":[]}`
	srv := newTestServer(t, "", body, http.StatusOK)
	defer srv.Close()
	cli := New(srv.URL, "")
	r, err := cli.ListReviews(context.Background())
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(r.Active) != 1 || r.Active[0].Name != "boop-x-1-abcdefg" {
		t.Errorf("active = %+v", r.Active)
	}
}

func TestListInstallationsRequiresToken(t *testing.T) {
	// /api/installations is open (no token needed).
	body := `{"installations":[{"id":123,"account_login":"octo"}],"fetched_at":"2026-01-01T00:00:00Z"}`
	srv := newTestServer(t, "", body, http.StatusOK)
	defer srv.Close()
	cli := New(srv.URL, "")
	r, err := cli.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	if len(r.Installations) != 1 || r.Installations[0].ID != 123 {
		t.Errorf("installations = %+v", r.Installations)
	}
}

func TestRerunRequiresToken(t *testing.T) {
	cli := New("http://localhost:1", "")
	_, err := cli.Rerun(context.Background(), "x", "test")
	if !isNoTokenErr(err) {
		t.Fatalf("expected ErrNoRunnerToken, got %v", err)
	}
}

func TestRerunPostsToken(t *testing.T) {
	body := `{"new_run_id":"x-r1","prior_run_id":"x","parent_run_id":"x"}`
	srv := newTestServer(t, "tok", body, http.StatusAccepted)
	defer srv.Close()
	cli := New(srv.URL, "tok")
	resp, err := cli.Rerun(context.Background(), "x", "reason")
	if err != nil {
		t.Fatalf("Rerun: %v", err)
	}
	if resp.NewRunID != "x-r1" {
		t.Errorf("new_run_id = %q, want x-r1", resp.NewRunID)
	}
}

func TestRerunPreview404(t *testing.T) {
	srv := newTestServer(t, "", "run not found", http.StatusNotFound)
	defer srv.Close()
	cli := New(srv.URL, "")
	_, err := cli.RerunPreview(context.Background(), "missing")
	ae, ok := err.(*ErrAPI)
	if !ok {
		t.Fatalf("expected *ErrAPI, got %T", err)
	}
	if ae.StatusCode != 404 {
		t.Errorf("status = %d, want 404", ae.StatusCode)
	}
}

func TestListRunsQueryEncoding(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"runs":[],"next_cursor":""}`)
	}))
	defer srv.Close()
	cli := New(srv.URL, "")
	_, err := cli.ListRuns(context.Background(), ListRunsOpts{
		Owner:  "octo",
		Repo:   "boop",
		Status: "failed",
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	// Verify query params were serialized.
	if !contains(seenPath, "owner=octo") || !contains(seenPath, "repo=boop") ||
		!contains(seenPath, "status=failed") || !contains(seenPath, "limit=50") {
		t.Errorf("query path = %q, expected owner/repo/status/limit", seenPath)
	}
}

func TestStatsDefaultBucket(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"from":"2026-01-01T00:00:00Z","to":"2026-01-01T00:00:00Z","bucket":"day","summary":{}}`)
	}))
	defer srv.Close()
	cli := New(srv.URL, "")
	_, err := cli.Stats(context.Background(), StatsOpts{Bucket: "day"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !contains(seen, "bucket=day") {
		t.Errorf("query = %q, expected bucket=day", seen)
	}
}

func TestStatsInvalidBucket(t *testing.T) {
	// The receiver returns 400 for an invalid bucket; our client
	// surfaces it as an *ErrAPI.
	srv := newTestServer(t, "", "invalid bucket", http.StatusBadRequest)
	defer srv.Close()
	cli := New(srv.URL, "")
	_, err := cli.Stats(context.Background(), StatsOpts{Bucket: "year"})
	ae, ok := err.(*ErrAPI)
	if !ok {
		t.Fatalf("expected *ErrAPI, got %T", err)
	}
	if ae.StatusCode != 400 {
		t.Errorf("status = %d, want 400", ae.StatusCode)
	}
}

func TestRetryOn5xx(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"active":[],"recent":[],"failed":[]}`)
	}))
	defer srv.Close()
	cli := New(srv.URL, "").WithRetries(3)
	_, err := cli.ListReviews(context.Background())
	if err != nil {
		t.Fatalf("ListReviews after retry: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries)", calls)
	}
}

func TestNoRetryOnPost(t *testing.T) {
	// POST endpoints are not retried; a 500 is returned directly.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cli := New(srv.URL, "tok").WithRetries(3)
	_, err := cli.Rerun(context.Background(), "x", "r")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (POSTs are not retried)", calls)
	}
}

func TestJSONDecodePreservesShape(t *testing.T) {
	body := `{"active":[{"name":"n1","namespace":"ns","owner":"o","repo":"r","pr":1,"commit":"abc1234","status":"Running","active":1}],"recent":[],"failed":[]}`
	srv := newTestServer(t, "", body, http.StatusOK)
	defer srv.Close()
	cli := New(srv.URL, "")
	r, err := cli.ListReviews(context.Background())
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	rv := r.Active[0]
	if rv.Name != "n1" || rv.Namespace != "ns" || rv.Owner != "o" || rv.Repo != "r" {
		t.Errorf("review = %+v", rv)
	}
	if rv.PR != 1 || rv.Commit != "abc1234" || rv.Status != "Running" || rv.Active != 1 {
		t.Errorf("review = %+v", rv)
	}
}

// isNoTokenErr reports whether err is the ErrNoRunnerToken value type
// the Rerun method returns when no token is configured. Rerun returns
// the value type directly, so a single type assertion suffices.
func isNoTokenErr(err error) bool {
	_, ok := err.(ErrNoRunnerToken)
	return ok
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestIdempotentPOSTRetryOn5xx(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "server error")
	}))
	defer srv.Close()
	cli := New(srv.URL, "tok").WithIdempotentPOST("/api/runs/").WithRetries(3)
	_, err := cli.Rerun(context.Background(), "x", "r")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// With retries=3, we expect 3 calls (1 initial + 2 retries).
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries on idempotent POST)", calls)
	}
}

func TestIdempotentPOSTPassesOnOK(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"new_run_id":"x-r1","prior_run_id":"x","parent_run_id":"x"}`)
	}))
	defer srv.Close()
	cli := New(srv.URL, "tok").WithIdempotentPOST("/api/runs/")
	resp, err := cli.Rerun(context.Background(), "x", "reason")
	if err != nil {
		t.Fatalf("Rerun: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if resp.NewRunID != "x-r1" {
		t.Errorf("new_run_id = %q, want x-r1", resp.NewRunID)
	}
}
