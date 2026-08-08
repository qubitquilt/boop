package client_test

import (
	"encoding/json"
	"testing"
	"time"

	boopapi "github.com/michaelruelas/boop-cli/internal/api"
)

// TestReceiverTypesDecodeIntoCLITypes is a compatibility guard against
// the two critical bugs that were fixed: (1) the RunWithTelemetry
// structural mismatch (receiver embeds Run anonymously, CLI must too)
// and (2) the JSON field-name mismatch (receiver store types now have
// snake_case JSON tags). If anyone removes a JSON tag from a store
// type or restructures RunWithTelemetry, this test fails.
//
// We use raw JSON that mirrors what the receiver's dashboard.go would
// emit after the store type tag additions — snake_case keys, flat
// RunWithTelemetry shape.

func TestRunWithTelemetryDecodesFlatShape(t *testing.T) {
	// This is the shape the receiver produces: store.Run fields at
	// the top level (anonymous embed), telemetry nested.
	raw := `{
		"id": "boop-qubitquilt-boop-42-a1b2c3d",
		"owner": "qubitquilt",
		"repo": "boop",
		"pr_number": 42,
		"commit_sha": "a1b2c3d4e5f67890",
		"base_ref": "main",
		"review_number": 1,
		"installation_id": 12345,
		"status": "succeeded",
		"started_at": "2026-08-01T12:00:00Z",
		"ended_at": "2026-08-01T12:03:30Z",
		"duration_ms": 210000,
		"created_at": "2026-08-01T12:00:00Z",
		"updated_at": "2026-08-01T12:03:30Z",
		"telemetry": {
			"run_id": "boop-qubitquilt-boop-42-a1b2c3d",
			"model": "minimax/minimax-m3",
			"input_tokens": 500,
			"output_tokens": 200,
			"cost_usd": 0.123,
			"step_count": 7
		}
	}`

	var got boopapi.RunWithTelemetry
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Every promoted field must decode — this is the regression guard.
	if got.ID != "boop-qubitquilt-boop-42-a1b2c3d" {
		t.Errorf("ID = %q, want the job name", got.ID)
	}
	if got.Owner != "qubitquilt" {
		t.Errorf("Owner = %q, want qubitquilt", got.Owner)
	}
	if got.Repo != "boop" {
		t.Errorf("Repo = %q, want boop", got.Repo)
	}
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
	if got.CommitSHA != "a1b2c3d4e5f67890" {
		t.Errorf("CommitSHA = %q, want a1b2c3d4e5f67890", got.CommitSHA)
	}
	if got.Status != boopapi.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	if got.InstallationID != 12345 {
		t.Errorf("InstallationID = %d, want 12345", got.InstallationID)
	}
	if got.Telemetry.Model != "minimax/minimax-m3" {
		t.Errorf("Telemetry.Model = %q, want minimax/minimax-m3", got.Telemetry.Model)
	}
	if got.Telemetry.CostUSD != 0.123 {
		t.Errorf("Telemetry.CostUSD = %v, want 0.123", got.Telemetry.CostUSD)
	}
}

func TestStatsResponseDecodesSnakeCase(t *testing.T) {
	raw := `{
		"from": "2026-07-01T00:00:00Z",
		"to": "2026-08-01T00:00:00Z",
		"bucket": "day",
		"summary": {
			"total_runs": 100,
			"succeeded_runs": 90,
			"failed_runs": 10,
			"running_runs": 0,
			"success_rate": 0.9,
			"total_cost_usd": 12.34,
			"total_tokens": 500000,
			"avg_duration_ms": 120000,
			"p50_duration_ms": 110000,
			"p95_duration_ms": 180000,
			"unique_repos": 5,
			"unique_installs": 2
		},
		"buckets": [
			{"bucket_start":"2026-08-01T00:00:00Z","runs":10,"succeeded":9,"failed":1,"cost_usd":1.0,"input_tokens":100,"output_tokens":40}
		],
		"by_repo": [
			{"owner":"qubitquilt","repo":"boop","runs":20,"succeeded":18,"failed":2,"success_rate":0.9,"total_cost_usd":2.5,"last_run_at":"2026-07-31T12:00:00Z"}
		],
		"by_model": [
			{"model":"minimax/minimax-m3","runs":20,"total_cost_usd":2.5,"input_tokens":500,"output_tokens":200}
		]
	}`

	var got boopapi.StatsResponse
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// SummaryStats — every field must decode from snake_case.
	if got.Summary.TotalRuns != 100 {
		t.Errorf("Summary.TotalRuns = %d, want 100", got.Summary.TotalRuns)
	}
	if got.Summary.FailedRuns != 10 {
		t.Errorf("Summary.FailedRuns = %d, want 10", got.Summary.FailedRuns)
	}
	if got.Summary.TotalCostUSD != 12.34 {
		t.Errorf("Summary.TotalCostUSD = %v, want 12.34", got.Summary.TotalCostUSD)
	}
	if got.Summary.P50DurationMS != 110000 {
		t.Errorf("Summary.P50DurationMS = %d, want 110000", got.Summary.P50DurationMS)
	}
	if got.Summary.UniqueRepos != 5 {
		t.Errorf("Summary.UniqueRepos = %d, want 5", got.Summary.UniqueRepos)
	}
	// Buckets
	if len(got.Buckets) != 1 {
		t.Fatalf("len(Buckets) = %d, want 1", len(got.Buckets))
	}
	bp := got.Buckets[0]
	if bp.Runs != 10 || bp.Succeeded != 9 || bp.Failed != 1 {
		t.Errorf("bucket = %+v", bp)
	}
	if bp.InputTokens != 100 || bp.OutputTokens != 40 {
		t.Errorf("bucket tokens = in:%d out:%d", bp.InputTokens, bp.OutputTokens)
	}
	// by_repo
	if len(got.ByRepo) != 1 {
		t.Fatalf("len(ByRepo) = %d, want 1", len(got.ByRepo))
	}
	rr := got.ByRepo[0]
	if rr.Owner != "qubitquilt" || rr.Repo != "boop" {
		t.Errorf("by_repo = %+v", rr)
	}
	if rr.Runs != 20 || rr.TotalCostUSD != 2.5 {
		t.Errorf("by_repo = %+v", rr)
	}
	// by_model
	if len(got.ByModel) != 1 {
		t.Fatalf("len(ByModel) = %d, want 1", len(got.ByModel))
	}
	if got.ByModel[0].Model != "minimax/minimax-m3" {
		t.Errorf("model = %q", got.ByModel[0].Model)
	}
}

// TestCLITypeRoundTrip verifies that the CLI type, when marshaled to
// JSON and the receiver's store.Run fields have proper tags, produces
// snake_case keys. This guards against future tag drift on the CLI side.
func TestCLITypeRoundTrip(t *testing.T) {
	in := boopapi.RunWithTelemetry{
		Run: boopapi.Run{
			ID:         "x",
			Owner:      "o",
			PRNumber:   42,
			CommitSHA:  "deadbee",
			Status:     boopapi.StatusFailed,
			StartedAt:  time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			DurationMS: int64Ptr(5000),
		},
		Telemetry: boopapi.Telemetry{
			RunID:       "x",
			Model:       "minimax/m3",
			InputTokens: 100,
			CostUSD:     0.01,
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	// Keys must be snake_case (not PascalCase).
	for _, key := range []string{`"id":`, `"owner":`, `"pr_number":`, `"commit_sha":`, `"started_at":`, `"duration_ms":`, `"input_tokens":`, `"cost_usd":`} {
		if !contains(s, key) {
			t.Errorf("missing key %q in JSON: %s", key, s)
		}
	}
	// Run fields must be at the TOP level (not nested under "run").
	if contains(s, `"run":`) {
		t.Errorf("Run was nested under a 'run' key — should be flat: %s", s)
	}
}

func int64Ptr(i int64) *int64 { return &i }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestInstallationLensOptOut verifies that an empty LensOptOut slice
// is omitted from JSON (omitempty on []string with nil slice).
func TestInstallationLensOptOut(t *testing.T) {
	// With nil slice — should be omitted entirely.
	inst := boopapi.Installation{
		ID: 1, AccountLogin: "test", AccountType: "User",
	}
	b, _ := json.Marshal(inst)
	if contains(string(b), "lens_opt_out") {
		t.Errorf("nil slice rendered lens_opt_out: %s", string(b))
	}

	// With non-empty slice — should render.
	inst.LensOptOut = []string{"security", "deep"}
	b, _ = json.Marshal(inst)
	if !contains(string(b), "lens_opt_out") {
		t.Errorf("expected lens_opt_out in output: %s", string(b))
	}
	if !contains(string(b), "security") {
		t.Errorf("expected 'security' in lens_opt_out: %s", string(b))
	}
}
