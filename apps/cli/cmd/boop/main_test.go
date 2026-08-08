package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michaelruelas/boop-cli/internal/client"
)

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"--version"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if !strings.Contains(stdout.String(), "dev") {
		t.Errorf("stdout = %q, want version string", stdout.String())
	}
}

func TestVersionInAnyPosition(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"runs", "--version"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if !strings.Contains(stdout.String(), "dev") {
		t.Errorf("stdout = %q, want version string", stdout.String())
	}
}

func TestJSONErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "run not found")
	}))
	defer srv.Close()
	t.Setenv("BOOP_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	_ = runMain(c, []string{"--json", "runs", "get", "missing-run"})

	// The error is returned AND printed to stderr as JSON.
	if stdout.String() != "" {
		t.Logf("stdout: %s", stdout.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr.String())), &parsed); err != nil {
		t.Fatalf("stderr is not valid JSON: %q: %v", stderr.String(), err)
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("JSON missing 'error' key: %v", parsed)
	}
	status, _ := errObj["status"].(float64)
	if status != 404 {
		t.Errorf("error.status = %v, want 404", status)
	}
	body, _ := errObj["body"].(string)
	if !strings.Contains(body, "not found") {
		t.Errorf("error.body = %q, want 'not found'", body)
	}
}

func TestJSONFlagInAnyPosition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"active":[],"recent":[],"failed":[]}`)
	}))
	defer srv.Close()
	t.Setenv("BOOP_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"reviews", "--json"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if !strings.Contains(stdout.String(), "active") {
		t.Errorf("stdout = %q, want JSON output with 'active' key", stdout.String())
	}
}

func TestJSONFlagBeforeCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"active":[],"recent":[],"failed":[]}`)
	}))
	defer srv.Close()
	t.Setenv("BOOP_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"--json", "reviews"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if !strings.Contains(stdout.String(), "active") {
		t.Errorf("stdout = %q, want JSON output with 'active' key", stdout.String())
	}
}

func TestHelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"--help"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("stdout = %q, want usage text", stdout.String())
	}
}

func TestNoCommandGivesUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no command given") {
		t.Errorf("err = %v, want 'no command given'", err)
	}
}

func TestConfigWriteURLValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"config", "write", "--api-url", "not a url"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Errorf("err = %v, want 'invalid URL' error", err)
	}
}

func TestConfigWriteValidURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"config", "write", "--api-url", "http://example.com:8080"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if !strings.Contains(stdout.String(), "wrote") {
		t.Errorf("stdout = %q, want 'wrote' confirmation", stdout.String())
	}
}

func TestRunsGetUsesNewEndpoint(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"run-123","owner":"o","repo":"r","pr_number":1,"commit_sha":"a1b2c3d","status":"succeeded","started_at":"2026-08-01T12:00:00Z","created_at":"2026-08-01T12:00:00Z","updated_at":"2026-08-01T12:03:30Z","telemetry":{"run_id":"run-123"}}`)
	}))
	defer srv.Close()
	t.Setenv("BOOP_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"runs", "get", "run-123"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if seenPath != "/api/runs/run-123" {
		t.Errorf("request path = %q, want /api/runs/run-123", seenPath)
	}
	if !strings.Contains(stdout.String(), "run-123") {
		t.Errorf("stdout = %q, want run id in output", stdout.String())
	}
}

func TestVersionShort(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"--version", "--short"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if stdout.String() == "dev" {
		t.Errorf("stdout = %q, want short version (not 'dev')", stdout.String())
	}
}

func TestVersionShortInAnyPosition(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, stderr: &stderr}
	err := runMain(c, []string{"runs", "--version", "--short"})
	if err != nil {
		t.Fatalf("runMain: %v", err)
	}
	if stdout.String() == "dev" {
		t.Errorf("stdout = %q, want short version (not 'dev')", stdout.String())
	}
}

func TestShortVersionExtractsSHA(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v0.1.0-141-gdbeb110-dirty", "gdbeb110"},
		{"v0.1.0-141-gdbeb110", "gdbeb110"},
		{"v0.1.0", "v0.1.0"},
		{"dev", "dev"},
		{"gabc1234", "gabc1234"},
	}
	for _, tc := range tests {
		got := shortVersion(tc.input)
		if got != tc.want {
			t.Errorf("shortVersion(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestClientGetRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runs/test-id" {
			t.Errorf("path = %q, want /api/runs/test-id", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"test-id","status":"succeeded","started_at":"2026-08-01T12:00:00Z","created_at":"2026-08-01T12:00:00Z","updated_at":"2026-08-01T12:03:30Z"}`)
	}))
	defer srv.Close()
	cli := client.New(srv.URL, "")
	run, err := cli.GetRun(context.Background(), "test-id")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.ID != "test-id" {
		t.Errorf("run.ID = %q, want test-id", run.ID)
	}
	if run.Status != "succeeded" {
		t.Errorf("run.Status = %q, want succeeded", run.Status)
	}
}