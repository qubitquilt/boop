package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	boopapi "github.com/michaelruelas/boop-cli/internal/api"
)

// renderJSON is a helper that marshals v via Render with asJSON=true
// and returns the output string.
func renderJSON(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, v, true); err != nil {
		t.Fatalf("Render JSON: %v", err)
	}
	return buf.String()
}

func renderHumanOut(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, v, false); err != nil {
		t.Fatalf("Render human: %v", err)
	}
	out := buf.String()
	t.Logf("human output: %q", out)
	return out
}

func TestRenderHealth(t *testing.T) {
	out := renderHumanOut(t, &boopapi.Health{Status: "ok"})
	if !strings.Contains(out, "ok") {
		t.Errorf("human output = %q, want 'ok'", out)
	}
	j := renderJSON(t, &boopapi.Health{Status: "ok"})
	if !strings.Contains(j, "ok") {
		t.Errorf("JSON output = %q, want 'ok'", j)
	}
}

func TestRenderReviewsBuckets(t *testing.T) {
	r := &boopapi.ReviewsResponse{
		Active: []boopapi.Review{{
			Name: "boop-x-1-a1b2c3d", Status: "Running",
			Owner: "x", Repo: "r", PR: 1, Commit: "a1b2c3d4e5f",
		}},
		Recent: []boopapi.Review{{
			Name: "boop-x-1-abcdefg", Status: "Complete",
			Owner: "x", Repo: "r", PR: 2, Commit: "abcdefg123456",
		}},
	}
	out := renderHumanOut(t, r)
	// Both buckets should render.
	if !strings.Contains(out, "ACTIVE") || !strings.Contains(out, "RECENT") {
		t.Errorf("human output missing buckets:\n%s", out)
	}
	// The active review's name should appear.
	if !strings.Contains(out, "boop-x-1-a1b2c3d") {
		t.Errorf("human output missing active review name:\n%s", out)
	}
}

func TestRenderReviewsEmptyOmitsSections(t *testing.T) {
	r := &boopapi.ReviewsResponse{}
	out := renderHumanOut(t, r)
	// No ACTIVE/RECENT/FAILED headers when all buckets are empty.
	if strings.Contains(out, "ACTIVE") || strings.Contains(out, "RECENT") || strings.Contains(out, "FAILED") {
		t.Errorf("expected no bucket headers for empty response, got:\n%s", out)
	}
}

func TestRenderRunsList(t *testing.T) {
	dur := int64(5000)
	r := &boopapi.ListRunsResponse{
		Runs: []boopapi.RunWithTelemetry{{
			Run: boopapi.Run{
				ID: "r1", Owner: "octo", Repo: "boop", PRNumber: 7,
				CommitSHA: "a1b2c3d4e5f67890", Status: boopapi.StatusSucceeded,
				StartedAt:  time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
				DurationMS: &dur,
			},
			Telemetry: boopapi.Telemetry{Model: "minimax/m3", CostUSD: 0.05},
		}},
		NextCursor: "2026-08-01T12:00:00Z|r1-next",
	}
	out := renderHumanOut(t, r)
	if !strings.Contains(out, "r1") {
		t.Errorf("expected run id r1 in output:\n%s", out)
	}
	if !strings.Contains(out, "octo/boop") {
		t.Errorf("expected owner/repo in output:\n%s", out)
	}
	if !strings.Contains(out, "minimax/m3") {
		t.Errorf("expected model in output:\n%s", out)
	}
	if !strings.Contains(out, "0.05") {
		t.Errorf("expected cost in output:\n%s", out)
	}
	if !strings.Contains(out, "cursor") {
		t.Errorf("expected next_cursor hint in output:\n%s", out)
	}
}

func TestRenderRunWithTelemetryDetail(t *testing.T) {
	rw := &boopapi.RunWithTelemetry{
		Run: boopapi.Run{
			ID: "r1", Owner: "octo", Repo: "boop", PRNumber: 1,
			CommitSHA:    "deadbeefface",
			Status:       boopapi.StatusFailed,
			StartedAt:    time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			Reason:       "pull_request.opened",
			Error:        "timeout",
			FailureClass: "pod_oomkilled",
		},
	}
	out := renderHumanOut(t, rw)
	if !strings.Contains(out, "r1") {
		t.Errorf("expected run id in output:\n%s", out)
	}
	if !strings.Contains(out, "octo/boop") {
		t.Errorf("expected owner/repo in output:\n%s", out)
	}
	if !strings.Contains(out, "deadbee") {
		t.Errorf("expected short sha in output:\n%s", out)
	}
	if !strings.Contains(out, "timeout") || !strings.Contains(out, "pod_oomkilled") {
		t.Errorf("expected error + failure class in output:\n%s", out)
	}
	if strings.Contains(out, "model:") {
		// Telemetry is zero-value; should say "no telemetry recorded".
		if !strings.Contains(out, "no telemetry") {
			t.Errorf("expected telemetry note, got:\n%s", out)
		}
	}
}

func TestRenderStats(t *testing.T) {
	durPtr := func(i int64) *int64 { return &i }
	s := &boopapi.StatsResponse{
		From:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Bucket: boopapi.BucketDay,
		Summary: boopapi.SummaryStats{
			TotalRuns:      100,
			SucceededRuns:  90,
			FailedRuns:     10,
			RunningRuns:    0,
			SuccessRate:    0.9,
			TotalCostUSD:   12.34,
			TotalTokens:    500000,
			AvgDurationMS:  120000,
			P50DurationMS:  110000,
			P95DurationMS:  180000,
			UniqueRepos:    5,
			UniqueInstalls: 2,
		},
		Buckets: []boopapi.BucketPoint{{
			BucketStart:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Runs:         10,
			Succeeded:    9,
			Failed:       1,
			CostUSD:      1.20,
			InputTokens:  300,
			OutputTokens: 200,
		}},
		ByRepo: []boopapi.RepoRollup{{
			Owner: "octo", Repo: "boop", Runs: 20,
			Succeeded: 18, Failed: 2, SuccessRate: 0.9,
			TotalCostUSD: 2.5, LastRunAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		}},
		ByModel: []boopapi.ModelRollup{{
			Model: "minimax/m3", Runs: 20, TotalCostUSD: 2.5,
			InputTokens: 500, OutputTokens: 200,
		}},
	}
	out := renderHumanOut(t, s)
	if !strings.Contains(out, "100") {
		t.Errorf("expected total runs in output:\n%s", out)
	}
	if !strings.Contains(out, "$12.34") {
		t.Errorf("expected total cost in output:\n%s", out)
	}
	if !strings.Contains(out, "minimax/m3") {
		t.Errorf("expected model in output:\n%s", out)
	}
	if !strings.Contains(out, "octo/boop") {
		t.Errorf("expected repo in output:\n%s", out)
	}
	_ = durPtr
}

func TestRenderRerunPreview(t *testing.T) {
	r := &boopapi.RerunPreviewResponse{
		Prior: boopapi.RerunPreviewRun{
			RunID: "r1", Status: "Failed", Model: "minimax/m3",
			HeadSHA: "a1b2c3d4e5f67890", Duration: 5000,
		},
		New: boopapi.RerunPreviewRun{
			RunID: "r1-r1", Status: "Pending", Model: "minimax/m3",
			HeadSHA: "a1b2c3d4e5f67890",
		},
	}
	out := renderHumanOut(t, r)
	if !strings.Contains(out, "r1") || !strings.Contains(out, "r1-r1") {
		t.Errorf("expected both prior and new run ids:\n%s", out)
	}
}

func TestRenderRerunResponse(t *testing.T) {
	r := &boopapi.RerunResponse{
		NewRunID:    "r1-r1",
		PriorRunID:  "r1",
		ParentRunID: "r1",
	}
	out := renderHumanOut(t, r)
	if !strings.Contains(out, "r1-r1") || !strings.Contains(out, "r1") {
		t.Errorf("expected run ids in output:\n%s", out)
	}
	// JSON output should have the keys.
	j := renderJSON(t, r)
	if !strings.Contains(j, "new_run_id") || !strings.Contains(j, "prior_run_id") {
		t.Errorf("JSON missing keys:\n%s", j)
	}
}

func TestRenderInstallations(t *testing.T) {
	r := &boopapi.InstallationsResponse{
		Installations: []boopapi.Installation{{
			ID: 12345, AccountLogin: "octo", AccountType: "User",
			RepositorySelection: "all",
			Paused:              false, FetchedAt: time.Now().UTC(),
		}},
		FetchedAt: time.Now().UTC(),
	}
	out := renderHumanOut(t, r)
	if !strings.Contains(out, "12,345") {
		t.Errorf("output missing installation id; see log for full output")
	}
	if !strings.Contains(out, "octo") {
		t.Errorf("output missing account login; see log for full output")
	}
}

func TestRenderStatsJSONShape(t *testing.T) {
	s := &boopapi.StatsResponse{
		Summary: boopapi.SummaryStats{TotalRuns: 5, TotalCostUSD: 1.50},
		ByRepo:  []boopapi.RepoRollup{{Owner: "o", Repo: "r", Runs: 5}},
	}
	j := renderJSON(t, s)
	if !strings.Contains(j, "total_runs") {
		t.Errorf("JSON missing snake_case key total_runs:\n%s", j)
	}
	if !strings.Contains(j, "total_cost_usd") {
		t.Errorf("JSON missing snake_case key total_cost_usd:\n%s", j)
	}
}

func TestRenderReviewJSONSnakeCase(t *testing.T) {
	r := &boopapi.Review{
		Name: "boop-x-1-a1b2c3d", Owner: "x", Repo: "r",
		PR: 1, Commit: "a1b2c3d4", Status: "Running",
		StartTime: "2026-08-01T12:00:00Z",
		BaseRef:   "main",
	}
	j := renderJSON(t, r)
	// The receiver's Review struct uses camelCase keys (baseRef, startTime).
	if !strings.Contains(j, "baseRef") {
		t.Errorf("JSON missing key baseRef:\n%s", j)
	}
}
