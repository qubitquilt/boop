// Package client is a thin HTTP client for the boop receiver API.
//
// It is intentionally minimal: it knows how to speak to the receiver's
// JSON endpoints, retry on transient failures, and translate HTTP error
// bodies into typed errors so the CLI's cobra commands can render
// human-readable messages without re-parsing.
//
// The receiver returns plain text ("kube error", "store error") on
// failure, so we capture the body verbatim in the typed error and let
// the command layer decide whether to print it.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/michaelruelas/boop-cli/internal/api"
)

// Timeout is the default per-request timeout. Generous: a stats query
// over a 365-day window can take a few seconds on a cold SQLite cache.
const DefaultTimeout = 30 * time.Second

// MaxRetries is the default number of retry attempts for idempotent
// GETs on transient errors (5xx, connection reset, context deadline).
const DefaultMaxRetries = 3

// ErrAPI wraps a receiver error response with the status code and body
// so callers can do errors.Is / errors.As without re-decoding. The CLI
// uses this to decide whether to print the body as a hint.
type ErrAPI struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *ErrAPI) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("boop: HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
	}
	return fmt.Sprintf("boop: HTTP %d", e.StatusCode)
}

// ErrNoRunnerToken is returned by commands that require the runner
// token when it is unset in config + env. Separated from ErrAPI so the
// error message can point the user at `boop config write` instead of
// "HTTP 401".
type ErrNoRunnerToken struct{}

func (e ErrNoRunnerToken) Error() string {
	return "boop: runner token is required for this action (set BOOP_RUNNER_TOKEN or run `boop config write` with runner_token)"
}

// Client is the receiver API client. Each field maps to a receiver
// endpoint; the methods return the decoded response type so the CLI
// does not deal with raw JSON on the happy path.
type Client struct {
	baseURL    string
	token      string
	http       *http.Client
	maxRetries int
}

// New constructs a Client from a base URL and optional runner token.
// A blank token is allowed — only the runner-only POST endpoints
// reject it at request time via ErrNoRunnerToken.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		http:       &http.Client{Timeout: DefaultTimeout},
		maxRetries: DefaultMaxRetries,
	}
}

// WithHTTP lets tests / advanced callers swap the transport. The CLI
// uses a default client; tests inject an httptest.Server client.
func (c *Client) WithHTTP(h *http.Client) *Client {
	cc := *c
	cc.http = h
	return &cc
}

// WithRetries overrides the retry budget. Only GETs are retried; the
// POST endpoints are not retried because they are not idempotent
// (telemetry is, telemetry-replace; status/stage are append-mostly).
func (c *Client) WithRetries(n int) *Client {
	cc := *c
	cc.maxRetries = n
	return &cc
}

// do is the retry-backed request runner. Retries only GETs on 5xx or
// net errors; records the last error (or 200 body) on success.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u := c.baseURL + path
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return nil, err
		}
		if c.token != "" {
			req.Header.Set("X-BOOP-Runner-Token", c.token)
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if method == http.MethodGet && isTransientErr(err) {
				// Brief backoff before retrying. No backoff
				// beyond this because the receiver is a
				// local service and exponential backoff
				// adds latency a human will notice.
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
				continue
			}
			return nil, err
		}
		if resp.StatusCode < 500 || method != http.MethodGet {
			return resp, nil
		}
		// Transient 5xx on a GET: retry.
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		resp.Body.Close()
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("exhausted %d retries", c.maxRetries)
	}
	return nil, lastErr
}

// isTransientErr reports whether err is a network-level error that a
// retry is likely to clear (connection reset, DNS hiccup, etc.).
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host")
}

// handleErr unwraps a non-2xx response into an *ErrAPI with the body
// preserved. The caller returns this so cobra renders a clean message.
func handleErr(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return &ErrAPI{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       strings.TrimSpace(string(body)),
	}
}

// decodeJSON is the happy-path decoder shared by every endpoint. On
// non-2xx it returns handleErr.
func (c *Client) decodeJSON(ctx context.Context, method, path string, body io.Reader, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return handleErr(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Health checks GET /health. Used by `boop health`.
func (c *Client) Health(ctx context.Context) (*api.Health, error) {
	var h api.Health
	if err := c.decodeJSON(ctx, http.MethodGet, "/health", nil, &h); err != nil {
		return nil, err
	}
	h.Status = "ok"
	return &h, nil
}

// ListReviews does GET /api/reviews.
func (c *Client) ListReviews(ctx context.Context) (*api.ReviewsResponse, error) {
	var out api.ReviewsResponse
	if err := c.decodeJSON(ctx, http.MethodGet, "/api/reviews", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListInstallations does GET /api/installations.
func (c *Client) ListInstallations(ctx context.Context) (*api.InstallationsResponse, error) {
	var out api.InstallationsResponse
	if err := c.decodeJSON(ctx, http.MethodGet, "/api/installations", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRunsOpts holds the query params for GET /api/runs.
type ListRunsOpts struct {
	Owner   string
	Repo    string
	Status  string
	Install int64
	From    time.Time
	To      time.Time
	Cursor  string
	Limit   int
}

// ListRuns does GET /api/runs with the given filters. Zero-value
// fields are omitted from the query string.
func (c *Client) ListRuns(ctx context.Context, o ListRunsOpts) (*api.ListRunsResponse, error) {
	q := url.Values{}
	if o.Owner != "" {
		q.Set("owner", o.Owner)
	}
	if o.Repo != "" {
		q.Set("repo", o.Repo)
	}
	if o.Status != "" {
		q.Set("status", o.Status)
	}
	if o.Install != 0 {
		q.Set("installation", fmt.Sprint(o.Install))
	}
	if !o.From.IsZero() {
		q.Set("from", o.From.Format(time.RFC3339))
	}
	if !o.To.IsZero() {
		q.Set("to", o.To.Format(time.RFC3339))
	}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit > 0 {
		q.Set("limit", fmt.Sprint(o.Limit))
	}
	path := "/api/runs"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out api.ListRunsResponse
	if err := c.decodeJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StatsOpts holds the query params for GET /api/stats.
type StatsOpts struct {
	From   time.Time
	To     time.Time
	Bucket string
}

// Stats does GET /api/stats.
func (c *Client) Stats(ctx context.Context, o StatsOpts) (*api.StatsResponse, error) {
	q := url.Values{}
	if !o.From.IsZero() {
		q.Set("from", o.From.Format(time.RFC3339))
	}
	if !o.To.IsZero() {
		q.Set("to", o.To.Format(time.RFC3339))
	}
	if o.Bucket != "" {
		q.Set("bucket", o.Bucket)
	}
	path := "/api/stats"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out api.StatsResponse
	if err := c.decodeJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RerunPreview does GET /api/runs/{id}/rerun-preview.
func (c *Client) RerunPreview(ctx context.Context, runID string) (*api.RerunPreviewResponse, error) {
	var out api.RerunPreviewResponse
	if err := c.decodeJSON(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(runID)+"/rerun-preview", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rerun does POST /api/runs/{id}/rerun. Requires the runner token.
func (c *Client) Rerun(ctx context.Context, runID, reason string) (*api.RerunResponse, error) {
	if c.token == "" {
		return nil, ErrNoRunnerToken{}
	}
	body, err := json.Marshal(api.RerunRequest{Confirm: true, Reason: reason})
	if err != nil {
		return nil, err
	}
	var out api.RerunResponse
	if err := c.decodeJSON(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(runID)+"/rerun", bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FetchPodLogs does a best-effort GET of the run's pod logs via the
// dashboard's on-demand fetcher. The receiver does not expose a public
// /api/runs/{id}/logs endpoint; this is a placeholder that the CLI
// implements by shelling out to kubectl when the user is port-forwarded
// in. (The receiver's FetchPodLogs is wired to the dashboard package,
// not the public API.) Returned as a string so the CLI can tee it to
// stdout.
//
// NOTE: This is intentionally a no-op stub in the client — the CLI's
// `runs logs` command shells out to kubectl directly (it needs the
// kube client which the receiver holds, not a public endpoint). This
// method exists for signature symmetry and may be revisited if the
// receiver gains a logs endpoint.
func (c *Client) FetchPodLogs(ctx context.Context, jobName string) (string, error) {
	return "", errors.New("client: pod logs are fetched via `kubectl logs`, not the API")
}
