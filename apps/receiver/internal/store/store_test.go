package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a file-backed store. Each test gets a
// fresh database; the file is created in t.TempDir() so the
// test framework cleans it up. The path is passed as a raw
// filesystem path (the QUB-101 canonical form); store.Open
// builds the DSN internally so a misconfigured test cannot
// silently drop a pragma.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boop.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// sampleRun builds a Run with sensible defaults so each test
// only specifies the fields it cares about. The id is derived
// from owner/repo/number/sha so a test can refer to it back
// without copy-pasting the same string.
func sampleRun(id, owner, repo string, pr int, sha string, status RunStatus, startedAt time.Time) Run {
	return Run{
		ID:             id,
		Owner:          owner,
		Repo:           repo,
		PRNumber:       pr,
		CommitSHA:      sha,
		BaseRef:        "main",
		ReviewNumber:   1,
		Reason:         "pull_request.opened",
		InstallationID: 12345,
		Status:         status,
		StartedAt:      startedAt,
	}
}

func TestUpsertRun_InsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	in := sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, now)
	got, err := s.UpsertRun(ctx, in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.ID != in.ID {
		t.Errorf("id = %q, want %q", got.ID, in.ID)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want %q", got.Status, StatusRunning)
	}
	if !got.StartedAt.Equal(now) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, now)
	}
}

func TestUpsertRun_UpdatePreservesImmutable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	first := sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, now)
	if _, err := s.UpsertRun(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// The second call changes mutable fields. Owner/repo/SHA
	// must not move — they belong to the original run.
	ended := now.Add(2 * time.Minute)
	dur := int64(120_000)
	second := Run{
		ID:         first.ID,
		Owner:      "DIFFERENT-OWNER", // should be ignored
		Repo:       "DIFFERENT-REPO",
		PRNumber:   999,
		CommitSHA:  "DIFFERENT-SHA",
		Status:     StatusSucceeded,
		StartedAt:  now.Add(time.Hour), // also ignored
		EndedAt:    &ended,
		DurationMS: &dur,
	}
	got, err := s.UpsertRun(ctx, second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got.Owner != "a" {
		t.Errorf("owner changed: %q", got.Owner)
	}
	if got.Repo != "b" {
		t.Errorf("repo changed: %q", got.Repo)
	}
	if got.PRNumber != 1 {
		t.Errorf("pr changed: %d", got.PRNumber)
	}
	if got.CommitSHA != "aaaaaaa" {
		t.Errorf("sha changed: %q", got.CommitSHA)
	}
	if got.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
	if !got.StartedAt.Equal(now) {
		t.Errorf("started_at moved: %v", got.StartedAt)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Errorf("ended_at = %v, want %v", got.EndedAt, ended)
	}
	if got.DurationMS == nil || *got.DurationMS != dur {
		t.Errorf("duration = %v, want %d", got.DurationMS, dur)
	}
}

func TestUpdateRunStatus_Narrow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ended := now.Add(90 * time.Second)
	dur := int64(90_000)
	got, err := s.UpdateRunStatus(ctx, "boop-a-b-1-aaaaaaa", StatusSucceeded, &ended, &dur, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Errorf("status = %q", got.Status)
	}
	if !got.StartedAt.Equal(now) {
		t.Errorf("started_at moved: %v", got.StartedAt)
	}
}

func TestUpdateRunStatus_UnknownRun(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpdateRunStatus(context.Background(), "boop-a-b-1-xxxxxxx", StatusSucceeded, nil, nil, "")
	if err == nil {
		t.Fatal("expected error for unknown run")
	}
}

func TestUpdateRunStatusIfRunning_OnlyWritesWhenRunning(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ended := now.Add(60 * time.Second)
	dur := int64(60_000)
	written, err := s.UpdateRunStatusIfRunning(ctx, "boop-a-b-1-aaaaaaa", StatusFailed, &ended, &dur, "reconciled")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !written {
		t.Fatal("expected written=true on running row")
	}
	got, err := s.GetRun(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Error != "reconciled" {
		t.Errorf("error = %q, want reconciled", got.Error)
	}

	// Second call on the now-terminal row should be a no-op.
	written, err = s.UpdateRunStatusIfRunning(ctx, "boop-a-b-1-aaaaaaa", StatusSucceeded, &ended, &dur, "late tick")
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if written {
		t.Fatal("expected written=false on terminal row")
	}
	got, err = s.GetRun(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status overwritten: %q", got.Status)
	}
	if got.Error != "reconciled" {
		t.Errorf("error overwritten: %q", got.Error)
	}
}

func TestUpdateRunStatusIfRunning_UnknownRunReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	written, err := s.UpdateRunStatusIfRunning(context.Background(), "no-such-run", StatusFailed, nil, nil, "")
	if err != nil {
		t.Fatalf("expected no error for unknown run, got %v", err)
	}
	if written {
		t.Fatal("expected written=false for unknown run")
	}
}

func TestMarkOrphanedRuns_OnlyOldNoHeartbeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.Add(-10 * time.Minute) // outside the 5m grace window
	fresh := now.Add(-2 * time.Minute) // inside the 5m grace window
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, old)); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-2-bbbbbbb", "a", "b", 2, "bbbbbbb", StatusRunning, fresh)); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	// Heartbeated row, old — must NOT be marked.
	heartbeated := sampleRun("boop-a-b-3-ccccccc", "a", "b", 3, "ccccccc", StatusRunning, old)
	if _, err := s.UpsertRun(ctx, heartbeated); err != nil {
		t.Fatalf("seed heartbeated: %v", err)
	}
	if err := s.TouchRunHeartbeat(ctx, heartbeated.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	n, err := s.MarkOrphanedRuns(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if n != 1 {
		t.Errorf("marked = %d, want 1", n)
	}

	// The old, no-heartbeat row is now failed with the orphan error.
	got, err := s.GetRun(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("old status = %q, want failed", got.Status)
	}
	if got.Error == "" || got.Error[:8] != "orphaned" {
		t.Errorf("old error = %q, want orphan prefix", got.Error)
	}

	// The fresh and heartbeated rows are untouched.
	for _, id := range []string{"boop-a-b-2-bbbbbbb", "boop-a-b-3-ccccccc"} {
		got, err := s.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != StatusRunning {
			t.Errorf("%s status = %q, want running", id, got.Status)
		}
	}
}

func TestMarkOrphanedRuns_NoMatches(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, time.Now().UTC())); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := s.MarkOrphanedRuns(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if n != 0 {
		t.Errorf("marked = %d, want 0", n)
	}
}

func TestListRuns_FilterByOwnerRepo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for i, tc := range []struct {
		id    string
		owner string
		repo  string
		pr    int
		at    time.Time
	}{
		{"boop-a-b-1-aaaaaaa", "a", "b", 1, base},
		{"boop-a-b-2-bbbbbbb", "a", "b", 2, base.Add(time.Minute)},
		{"boop-a-c-1-ccccccc", "a", "c", 1, base.Add(2 * time.Minute)},
		{"boop-x-y-1-ddddddd", "x", "y", 1, base.Add(3 * time.Minute)},
	} {
		r := sampleRun(tc.id, tc.owner, tc.repo, tc.pr, string(rune('a'+i))+"aaaaaaa", StatusSucceeded, tc.at)
		if _, err := s.UpsertRun(ctx, r); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// owner=a should return three runs, newest first
	got, err := s.ListRuns(ctx, ListRunsFilter{Owner: "a", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got.Runs) != 3 {
		t.Fatalf("len = %d, want 3", len(got.Runs))
	}
	// Newest within owner=a is a/c (added at base+2m). The x/y
	// run is at base+3m but filtered out by owner=a.
	if got.Runs[0].ID != "boop-a-c-1-ccccccc" {
		t.Errorf("newest first wrong: %s", got.Runs[0].ID)
	}

	// owner=a, repo=b should return two
	got, err = s.ListRuns(ctx, ListRunsFilter{Owner: "a", Repo: "b", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got.Runs) != 2 {
		t.Errorf("len = %d, want 2", len(got.Runs))
	}
}

func TestListRuns_KeysetPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 6; i++ {
		r := sampleRun(
			"boop-a-b-"+string(rune('1'+i))+"-"+string(rune('a'+i))+"aaaaaa",
			"a", "b", i+1,
			string(rune('a'+i))+"aaaaaa",
			StatusSucceeded,
			base.Add(time.Duration(i)*time.Minute),
		)
		if _, err := s.UpsertRun(ctx, r); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	page1, err := s.ListRuns(ctx, ListRunsFilter{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Runs) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Runs))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 missing cursor")
	}

	page2, err := s.ListRuns(ctx, ListRunsFilter{Cursor: page1.NextCursor, Limit: 2})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Runs) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2.Runs))
	}
	// Pages must not overlap.
	for _, r1 := range page1.Runs {
		for _, r2 := range page2.Runs {
			if r1.ID == r2.ID {
				t.Errorf("page overlap: %s", r1.ID)
			}
		}
	}

	// page3 should have the last 2 items; the third call has no next cursor.
	page3, err := s.ListRuns(ctx, ListRunsFilter{Cursor: page2.NextCursor, Limit: 2})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3.Runs) != 2 {
		t.Errorf("page3 len = %d, want 2", len(page3.Runs))
	}
	if page3.NextCursor != "" {
		t.Errorf("page3 should be last; got next cursor %q", page3.NextCursor)
	}
}

// SP-006: the bulk fetch returns the page with each run
// paired with its telemetry. Runs without a telemetry row
// get a zero-valued Telemetry (not a hard error) so the
// dashboard's table renders the row without a $ column.
func TestListRunsWithTelemetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, now)); err != nil {
		t.Fatalf("seed run 1: %v", err)
	}
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-2-bbbbbbb", "a", "b", 2, "bbbbbbb", StatusFailed, now.Add(time.Minute))); err != nil {
		t.Fatalf("seed run 2: %v", err)
	}
	// Only the first run has telemetry.
	if err := s.RecordTelemetry(ctx, Telemetry{
		RunID:    "boop-a-b-1-aaaaaaa",
		Model:    "openrouter/x",
		CostUSD:  0.05,
		StepCount: 1,
	}); err != nil {
		t.Fatalf("seed telemetry: %v", err)
	}
	page, err := s.ListRunsWithTelemetry(ctx, ListRunsFilter{Owner: "a", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(page.Runs))
	}
	var withT, withoutT *RunWithTelemetry
	for i := range page.Runs {
		if page.Runs[i].Run.ID == "boop-a-b-1-aaaaaaa" {
			withT = &page.Runs[i]
		} else {
			withoutT = &page.Runs[i]
		}
	}
	if withT == nil || withT.Telemetry.CostUSD != 0.05 {
		t.Errorf("run-with-telemetry cost = %v, want 0.05", withT)
	}
	if withoutT == nil || withoutT.Telemetry.RunID != "" {
		t.Errorf("run-without-telemetry: %+v, want zero Telemetry", withoutT)
	}
}

func TestRecordTelemetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reqID := "chatcmpl-1"
	dur := int64(4321)
	telem := Telemetry{
		RunID:               "boop-a-b-1-aaaaaaa",
		Model:               "openrouter/anthropic/claude-3.5-sonnet",
		Provider:            "openrouter",
		InputTokens:         1000,
		OutputTokens:        500,
		TotalTokens:         1505,
		ReasoningTokens:     0,
		CacheReadTokens:     200,
		CacheWriteTokens:    0,
		CostUSD:             0.0123,
		CostPromptUSD:       0.001,
		CostCompletionUSD:   0.0113,
		CostUpstreamUSD:     0.0124,
		IsByok:              true,
		ServerToolCallsExec: 0,
		ServerToolCallsReq:  0,
		RequestID:           &reqID,
		DurationMS:          &dur,
		StepCount:           3,
		RecordedAt:          now,
	}
	if err := s.RecordTelemetry(ctx, telem); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.GetTelemetry(ctx, telem.RunID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != telem.Model {
		t.Errorf("model = %q", got.Model)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 500 || got.TotalTokens != 1505 {
		t.Errorf("tokens = %+v", got)
	}
	if got.CostUSD != 0.0123 {
		t.Errorf("cost = %f", got.CostUSD)
	}
	if got.CostPromptUSD != 0.001 || got.CostCompletionUSD != 0.0113 || got.CostUpstreamUSD != 0.0124 {
		t.Errorf("cost split = (%f, %f, %f)", got.CostPromptUSD, got.CostCompletionUSD, got.CostUpstreamUSD)
	}
	if !got.IsByok {
		t.Errorf("is_byok = false, want true")
	}
	if got.RequestID == nil || *got.RequestID != reqID {
		t.Errorf("request_id = %v, want %q", got.RequestID, reqID)
	}
	if got.DurationMS == nil || *got.DurationMS != dur {
		t.Errorf("duration_ms = %v, want %d", got.DurationMS, dur)
	}
}

func TestRecordTelemetry_DefaultsForMissingQUB105Fields(t *testing.T) {
	// QUB-105 acceptance: a pre-QUB-105 runner posts a partial
	// payload (no total_tokens, no request_id, no cost
	// split). The store lands a usable row with the SQL DEFAULT
	// values for the new scalar columns and NULL for the
	// nullable ones — the dashboard can render a row even when
	// the runner is still on the older contract.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	telem := Telemetry{
		RunID:        "boop-a-b-1-aaaaaaa",
		Model:        "m",
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.001,
		StepCount:    1,
		RecordedAt:   now,
	}
	if err := s.RecordTelemetry(ctx, telem); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.GetTelemetry(ctx, telem.RunID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalTokens != 0 {
		t.Errorf("total_tokens = %d, want 0", got.TotalTokens)
	}
	if got.CostPromptUSD != 0 || got.CostCompletionUSD != 0 || got.CostUpstreamUSD != 0 {
		t.Errorf("cost split not zeroed: %+v", got)
	}
	if got.IsByok {
		t.Errorf("is_byok = true, want false (default)")
	}
	if got.RequestID != nil {
		t.Errorf("request_id = %v, want nil", got.RequestID)
	}
	if got.DurationMS != nil {
		t.Errorf("duration_ms = %v, want nil", got.DurationMS)
	}
}

func TestRecordTelemetry_QUB105ErrorContext(t *testing.T) {
	// QUB-105: a failed SDK call stamps the human-readable
	// error string, status_code, content_type, and a short
	// body snippet on the telemetry row. The nullable columns
	// store NULL on success; the dashboard's failure-class
	// filter can read error_status_code (typed) for
	// programmatic decisions and error (string) for the
	// operator's breadcrumb.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusFailed, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	statusCode := int64(401)
	contentType := "application/json"
	errMsg := "OpenRouter chat completion failed (401): Bad token"
	errBody := `{"error":"unauthorized"}`
	telem := Telemetry{
		RunID:            "boop-a-b-1-aaaaaaa",
		Model:            "openrouter/x",
		StepCount:        1,
		RecordedAt:       now,
		Error:            &errMsg,
		ErrorStatusCode:  &statusCode,
		ErrorContentType: &contentType,
		ErrorBody:        &errBody,
	}
	if err := s.RecordTelemetry(ctx, telem); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.GetTelemetry(ctx, telem.RunID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Error == nil || *got.Error != errMsg {
		t.Errorf("error = %v, want %q", got.Error, errMsg)
	}
	if got.ErrorStatusCode == nil || *got.ErrorStatusCode != 401 {
		t.Errorf("error_status_code = %v, want 401", got.ErrorStatusCode)
	}
	if got.ErrorContentType == nil || *got.ErrorContentType != "application/json" {
		t.Errorf("error_content_type = %v, want application/json", got.ErrorContentType)
	}
	if got.ErrorBody == nil || *got.ErrorBody != errBody {
		t.Errorf("error_body = %v, want %q", got.ErrorBody, errBody)
	}
}

func TestRecordTelemetry_UnknownRun(t *testing.T) {
	// QUB-101: a runner POST that lands before the receiver's
	// UpsertRun has committed gets ErrUnknownRun back; the
	// handler matches the sentinel and returns 202 to the
	// runner. The runner does not retry, so the cost data is
	// lost — this is the documented edge case. The test pins
	// the contract: errors.Is(err, store.ErrUnknownRun) is the
	// check the handler relies on.
	s := newTestStore(t)
	err := s.RecordTelemetry(context.Background(), Telemetry{RunID: "missing", Model: "x"})
	if err == nil {
		t.Fatal("expected error for unknown run")
	}
	if !errors.Is(err, ErrUnknownRun) {
		t.Errorf("err = %v, want ErrUnknownRun", err)
	}
}

func TestRecordTelemetry_Replace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first := Telemetry{RunID: "boop-a-b-1-aaaaaaa", Model: "m1", InputTokens: 100, CostUSD: 0.01, StepCount: 1, RecordedAt: now}
	if err := s.RecordTelemetry(ctx, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := Telemetry{RunID: "boop-a-b-1-aaaaaaa", Model: "m2", InputTokens: 200, CostUSD: 0.02, StepCount: 2, RecordedAt: now.Add(time.Second)}
	if err := s.RecordTelemetry(ctx, second); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, err := s.GetTelemetry(ctx, "boop-a-b-1-aaaaaaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "m2" || got.InputTokens != 200 || got.CostUSD != 0.02 || got.StepCount != 2 {
		t.Errorf("replace failed: %+v", got)
	}
}

func TestSummary_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Summary(context.Background(), time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.TotalRuns != 0 {
		t.Errorf("total = %d, want 0", got.TotalRuns)
	}
	if got.SuccessRate != 0 {
		t.Errorf("success rate = %f, want 0", got.SuccessRate)
	}
}

func TestSummary_WithRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// 3 succeeded, 1 failed, 1 running
	cases := []struct {
		id     string
		status RunStatus
		dur    *int64
	}{
		{"boop-a-b-1-aaaaaaa", StatusSucceeded, ptrInt64(60_000)},
		{"boop-a-b-2-bbbbbbb", StatusSucceeded, ptrInt64(120_000)},
		{"boop-a-b-3-ccccccc", StatusSucceeded, ptrInt64(180_000)},
		{"boop-a-b-4-ddddddd", StatusFailed, ptrInt64(30_000)},
		{"boop-a-b-5-eeeeeee", StatusRunning, nil},
	}
	for i, tc := range cases {
		r := sampleRun(tc.id, "a", "b", i+1, string(rune('a'+i))+"aaaaaa", tc.status, base.Add(time.Duration(i)*time.Minute))
		r.DurationMS = tc.dur
		if _, err := s.UpsertRun(ctx, r); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	got, err := s.Summary(ctx, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.TotalRuns != 5 {
		t.Errorf("total = %d, want 5", got.TotalRuns)
	}
	if got.SucceededRuns != 3 {
		t.Errorf("succeeded = %d, want 3", got.SucceededRuns)
	}
	if got.FailedRuns != 1 {
		t.Errorf("failed = %d, want 1", got.FailedRuns)
	}
	if got.RunningRuns != 1 {
		t.Errorf("running = %d, want 1", got.RunningRuns)
	}
	if got.SuccessRate < 0.59 || got.SuccessRate > 0.61 {
		t.Errorf("success rate = %f, want ~0.6", got.SuccessRate)
	}
	// 3 durations: 60k, 120k, 180k. P50 -> 120k, P95 -> 180k.
	if got.P50DurationMS != 120_000 {
		t.Errorf("p50 = %d, want 120000", got.P50DurationMS)
	}
	if got.P95DurationMS != 180_000 {
		t.Errorf("p95 = %d, want 180000", got.P95DurationMS)
	}
	if got.UniqueRepos != 1 {
		t.Errorf("unique repos = %d, want 1", got.UniqueRepos)
	}
}

func TestPerRepo_OrderByRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// 3 runs in a/b, 1 in a/c
	ids := []string{
		"boop-a-b-1-aaaaaaa", "boop-a-b-2-bbbbbbb", "boop-a-b-3-ccccccc",
		"boop-a-c-1-ddddddd",
	}
	for i, id := range ids {
		repo := "b"
		if i == 3 {
			repo = "c"
		}
		if _, err := s.UpsertRun(ctx, sampleRun(id, "a", repo, i+1, id[len(id)-7:], StatusSucceeded, base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	rolls, err := s.PerRepo(ctx, base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("per repo: %v", err)
	}
	if len(rolls) != 2 {
		t.Fatalf("len = %d, want 2", len(rolls))
	}
	if rolls[0].Repo != "b" || rolls[0].Runs != 3 {
		t.Errorf("first roll = %+v", rolls[0])
	}
	if rolls[1].Repo != "c" || rolls[1].Runs != 1 {
		t.Errorf("second roll = %+v", rolls[1])
	}
}

func TestUpsertInstallations_Replace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	first := []Installation{
		{ID: 1, AccountLogin: "alice", AccountType: "User", FetchedAt: now},
		{ID: 2, AccountLogin: "org-b", AccountType: "Organization", FetchedAt: now},
	}
	if err := s.UpsertInstallations(ctx, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Second batch removes org-b.
	second := []Installation{
		{ID: 1, AccountLogin: "alice", AccountType: "User", FetchedAt: now.Add(time.Minute)},
	}
	if err := s.UpsertInstallations(ctx, second); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, err := s.ListInstallations(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].AccountLogin != "alice" {
		t.Errorf("got %+v, want only alice", got)
	}
	latest, err := s.LatestInstallationFetch(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if !latest.Equal(now.Add(time.Minute)) {
		t.Errorf("latest = %v, want %v", latest, now.Add(time.Minute))
	}
}

func TestOpen_AppliesPragmas(t *testing.T) {
	// QUB-101: every connection-time pragma must land
	// before the migration runs. A misconfigured DSN that
	// dropped any of these would surface here.
	s := newTestStore(t)
	ctx := context.Background()

	checks := []struct {
		name string
		want string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "2"}, // FULL == 2 in SQLite's enum
		{"foreign_keys", "1"},
		{"auto_vacuum", "2"}, // INCREMENTAL == 2
	}
	for _, c := range checks {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+c.name).Scan(&got); err != nil {
			t.Fatalf("pragma %s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("pragma %s = %q, want %q", c.name, got, c.want)
		}
	}
	var v int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != int64(currentSchemaVersion) {
		t.Errorf("user_version = %d, want %d", v, currentSchemaVersion)
	}
}

func TestOpen_IdempotentMigration(t *testing.T) {
	// Open a fresh store, then Open it again with the same
	// path. The second Open should be a no-op for the schema
	// (migrateV1 uses CREATE TABLE IF NOT EXISTS, so the
	// re-run is harmless), and user_version should stay at
	// currentSchemaVersion.
	path := filepath.Join(t.TempDir(), "boop.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s1.UpsertRun(context.Background(), sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusRunning, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	var v int64
	if err := s2.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != int64(currentSchemaVersion) {
		t.Errorf("user_version = %d, want %d (idempotent re-open should not bump)", v, currentSchemaVersion)
	}
	// The row from the first Open should still be there.
	if _, err := s2.GetRun(context.Background(), "boop-a-b-1-aaaaaaa"); err != nil {
		t.Errorf("row from first open missing: %v", err)
	}
}

func TestDeepCheck_FreshDB(t *testing.T) {
	s := newTestStore(t)
	ok, result, err := s.DeepCheck()
	if err != nil {
		t.Fatalf("deep check: %v", err)
	}
	if !ok {
		t.Errorf("ok = false, result = %q", result)
	}
	if result != "ok" {
		t.Errorf("result = %q, want %q", result, "ok")
	}
}

func TestStats_FreshDB(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.Runs != 0 || got.Telemetry != 0 {
		t.Errorf("fresh stats: %+v, want zero runs/telemetry", got)
	}
	if got.FileBytes <= 0 {
		t.Errorf("file_bytes = %d, want > 0", got.FileBytes)
	}
	if got.FreelistCount != 0 {
		t.Errorf("freelist_count = %d, want 0 on a fresh db", got.FreelistCount)
	}
}

func TestStats_WithRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := s.UpsertRun(ctx, sampleRun(
			"boop-a-b-"+string(rune('1'+i))+"-"+string(rune('a'+i))+"aaaaaa",
			"a", "b", i+1,
			string(rune('a'+i))+"aaaaaa",
			StatusSucceeded,
			now,
		)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	if err := s.RecordTelemetry(ctx, Telemetry{
		RunID:      "boop-a-b-1-aaaaaaa",
		Model:      "x",
		InputTokens: 1,
		OutputTokens: 1,
		CostUSD:    0.01,
		StepCount:  1,
		RecordedAt: now,
	}); err != nil {
		t.Fatalf("telemetry: %v", err)
	}

	got, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.Runs != 3 {
		t.Errorf("runs = %d, want 3", got.Runs)
	}
	if got.Telemetry != 1 {
		t.Errorf("telemetry = %d, want 1", got.Telemetry)
	}
}

func TestPruneRuns_DeletesOld(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Anchor to wall-clock now so the prune cutoff (also
	// wall-clock based) lines up with the row timestamps.
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour) // 100 days old
	recent := now.Add(-1 * time.Hour)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, old)); err != nil {
		t.Fatalf("old: %v", err)
	}
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-2-bbbbbbb", "a", "b", 2, "bbbbbbb", StatusSucceeded, recent)); err != nil {
		t.Fatalf("recent: %v", err)
	}

	// 30-day retention: old is deleted, recent survives.
	pruned, err := s.PruneRuns(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}
	if _, err := s.GetRun(ctx, "boop-a-b-1-aaaaaaa"); err == nil {
		t.Errorf("old run still present")
	}
	if _, err := s.GetRun(ctx, "boop-a-b-2-bbbbbbb"); err != nil {
		t.Errorf("recent run missing: %v", err)
	}
}

func TestPruneRuns_CascadesToTelemetry(t *testing.T) {
	// FK on telemetry.run_id is ON DELETE CASCADE; pruning
	// the parent must remove the child too. If foreign_keys
	// is not on (one of the QUB-101 pragmas), this test
	// would fail with a dangling telemetry row.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, old)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.RecordTelemetry(ctx, Telemetry{
		RunID:      "boop-a-b-1-aaaaaaa",
		Model:      "x",
		InputTokens: 1,
		CostUSD:    0.01,
		StepCount:  1,
		RecordedAt: now,
	}); err != nil {
		t.Fatalf("telemetry: %v", err)
	}

	if _, err := s.PruneRuns(ctx, 30*24*time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := s.GetTelemetry(ctx, "boop-a-b-1-aaaaaaa"); err == nil {
		t.Errorf("telemetry row survived parent prune (foreign_keys may be off)")
	}
}

func TestRunRetention_FirstTickSkipsVacuum(t *testing.T) {
	// The first retention tick after Open does not run
	// incremental_vacuum — we don't know how long it's been
	// since the previous one ran. The tick still prunes and
	// checkpoints the WAL.
	s := newTestStore(t)
	ctx := context.Background()
	res, err := s.RunRetention(ctx, 30*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Vacuumed {
		t.Errorf("first tick should not vacuum, got Vacuumed=true (pages=%d)", res.VacuumPages)
	}
	if !res.WALCheck {
		t.Errorf("first tick should checkpoint WAL")
	}
}

func TestRunRetention_PrunesAndCheckpoints(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := s.UpsertRun(ctx, sampleRun(
			"boop-a-b-"+string(rune('1'+i))+"-"+string(rune('a'+i))+"aaaaaa",
			"a", "b", i+1,
			string(rune('a'+i))+"aaaaaa",
			StatusSucceeded,
			old,
		)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	res, err := s.RunRetention(ctx, 30*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Pruned != 3 {
		t.Errorf("pruned = %d, want 3", res.Pruned)
	}
	stats, _ := s.Stats(ctx)
	if stats.Runs != 0 {
		t.Errorf("runs after prune = %d, want 0", stats.Runs)
	}
}

func ptrInt64(v int64) *int64 { return &v }
func ptr(t time.Time) *time.Time { return &t }
func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// QUB-110: lineage round-trip. parent_run_id and
// superseded_by_id are written via UpsertRun and read
// back through GetRun. The dashboard's "vertical
// timeline" view walks parent_run_id.
func TestUpsertRun_LineageRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prior := "boop-a-b-1-aaaaaaa"
	next := "boop-a-b-1-aaaaaaa-r1"
	if _, err := s.UpsertRun(ctx, sampleRun(prior, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert prior: %v", err)
	}
	if _, err := s.UpsertRun(ctx, Run{
		ID:           next,
		Owner:        "a",
		Repo:         "b",
		PRNumber:     1,
		CommitSHA:    "aaaaaaa",
		BaseRef:      "main",
		ReviewNumber: 2,
		Status:       StatusPending,
		StartedAt:    time.Now().UTC(),
		ParentRunID:  prior,
	}); err != nil {
		t.Fatalf("upsert next: %v", err)
	}
	if err := s.SetSupersededBy(ctx, prior, next); err != nil {
		t.Fatalf("set superseded: %v", err)
	}
	got, err := s.GetRun(ctx, next)
	if err != nil {
		t.Fatalf("get next: %v", err)
	}
	if got.ParentRunID != prior {
		t.Errorf("parent_run_id = %q, want %q", got.ParentRunID, prior)
	}
	priorRow, err := s.GetRun(ctx, prior)
	if err != nil {
		t.Fatalf("get prior: %v", err)
	}
	if priorRow.SupersededByID != next {
		t.Errorf("superseded_by_id = %q, want %q", priorRow.SupersededByID, next)
	}
}

// EH-005: CreateRerun is the atomic version of
// UpsertRun + SetSupersededBy. A crash between the two
// writes used to leave either a "no parent link" new row
// or a "points at a non-existent id" prior row. The
// transaction wraps both, so we can assert the post-
// commit state in one place.
func TestCreateRerun_AtomicLineage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prior := "boop-a-b-1-aaaaaaa"
	next := "boop-a-b-1-aaaaaaa-r1"
	if _, err := s.UpsertRun(ctx, sampleRun(prior, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert prior: %v", err)
	}
	got, err := s.CreateRerun(ctx, Run{
		ID:           next,
		Owner:        "a",
		Repo:         "b",
		PRNumber:     1,
		CommitSHA:    "aaaaaaa",
		BaseRef:      "main",
		ReviewNumber: 2,
		Status:       StatusPending,
		StartedAt:    time.Now().UTC(),
		ParentRunID:  prior,
	}, prior)
	if err != nil {
		t.Fatalf("create rerun: %v", err)
	}
	if got.ParentRunID != prior {
		t.Errorf("new row parent_run_id = %q, want %q", got.ParentRunID, prior)
	}
	if got.Status != StatusPending {
		t.Errorf("new row status = %q, want pending", got.Status)
	}
	priorRow, err := s.GetRun(ctx, prior)
	if err != nil {
		t.Fatalf("get prior: %v", err)
	}
	if priorRow.SupersededByID != next {
		t.Errorf("prior superseded_by_id = %q, want %q", priorRow.SupersededByID, next)
	}
}

// EH-005: a second CreateRerun call against the same
// prior must not clobber an existing superseded_by_id.
// The operator's "double-click on Requeue" surfaces here
// as two handler calls; the second should be a no-op on
// the prior (and would still insert the new row, since
// the new id differs).
func TestCreateRerun_DoesNotClobberExistingSupersede(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prior := "boop-a-b-1-aaaaaaa"
	first := "boop-a-b-1-aaaaaaa-r1"
	second := "boop-a-b-1-aaaaaaa-r2"
	if _, err := s.UpsertRun(ctx, sampleRun(prior, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert prior: %v", err)
	}
	if _, err := s.CreateRerun(ctx, Run{
		ID: first, Owner: "a", Repo: "b", PRNumber: 1, CommitSHA: "aaaaaaa",
		BaseRef: "main", ReviewNumber: 2, Status: StatusPending,
		StartedAt: time.Now().UTC(), ParentRunID: prior,
	}, prior); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateRerun(ctx, Run{
		ID: second, Owner: "a", Repo: "b", PRNumber: 1, CommitSHA: "aaaaaaa",
		BaseRef: "main", ReviewNumber: 3, Status: StatusPending,
		StartedAt: time.Now().UTC(), ParentRunID: prior,
	}, prior); err != nil {
		t.Fatalf("second create: %v", err)
	}
	// Prior's superseded_by_id stays pointed at the
	// first re-run; the second call's UPDATE sees the
	// existing non-empty value and skips.
	priorRow, err := s.GetRun(ctx, prior)
	if err != nil {
		t.Fatalf("get prior: %v", err)
	}
	if priorRow.SupersededByID != first {
		t.Errorf("prior superseded_by_id = %q, want %q (first)", priorRow.SupersededByID, first)
	}
}

// EH-008: an explicit candidate name that collides
// with an existing row returns ErrDuplicateRerunName.
// The handler's retry loop is the consumer; the store
// surfaces the failure rather than silently
// overwriting.
func TestCreateRerun_DuplicateNameReturnsError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	prior := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(prior, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert prior: %v", err)
	}
	first := "boop-a-b-1-aaaaaaa-r1"
	if _, err := s.CreateRerun(ctx, Run{
		ID: first, Owner: "a", Repo: "b", PRNumber: 1, CommitSHA: "aaaaaaa",
		BaseRef: "main", ReviewNumber: 2, Status: StatusPending,
		StartedAt: time.Now().UTC(), ParentRunID: prior,
	}, prior); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Second attempt with the same candidate name.
	_, err := s.CreateRerun(ctx, Run{
		ID: first, Owner: "a", Repo: "b", PRNumber: 1, CommitSHA: "aaaaaaa",
		BaseRef: "main", ReviewNumber: 3, Status: StatusPending,
		StartedAt: time.Now().UTC(), ParentRunID: prior,
	}, prior)
	if !errors.Is(err, ErrDuplicateRerunName) {
		t.Errorf("err = %v, want ErrDuplicateRerunName", err)
	}
}

// QUB-110: GetRun returns ErrUnknownRun for an unknown
// id. The handler uses this to map to 404 instead of
// 500.
func TestGetRun_UnknownReturnsErrUnknownRun(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetRun(context.Background(), "no-such-run")
	if !errors.Is(err, ErrUnknownRun) {
		t.Errorf("err = %v, want ErrUnknownRun", err)
	}
}

// QUB-110: CountRerunJobsForSHA returns 0 for a fresh
// head SHA and increments as re-runs land. The -r{n}
// suffix is what makes the count work — a Job name
// without the suffix (the original) does not match.
func TestCountRerunJobsForSHA_IncrementsOnReruns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	original := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(original, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert original: %v", err)
	}
	got, err := s.CountRerunJobsForSHA(ctx, "a", "b", 1, "aaaaaaa")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0 (no reruns yet)", got)
	}
	if _, err := s.UpsertRun(ctx, Run{
		ID: "boop-a-b-1-aaaaaaa-r1", Owner: "a", Repo: "b", PRNumber: 1,
		CommitSHA: "aaaaaaa", BaseRef: "main", Status: StatusSucceeded,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert r1: %v", err)
	}
	got, _ = s.CountRerunJobsForSHA(ctx, "a", "b", 1, "aaaaaaa")
	if got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

// QUB-112: audit log. Every dashboard-initiated action
// appends a row. The actor column is the load-bearing
// piece — a faceless action is unattributable.
func TestRecordAuditEvent_AppendOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i, action := range []string{"rerun.create", "install.pause", "cost.zero_out"} {
		ev, err := s.RecordAuditEvent(ctx, AuditEvent{
			Action:   action,
			Actor:    "tester",
			TargetID: "boop-a-b-1-aaaaaaa",
			Details:  `{"reason":"unit test"}`,
		})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if ev.ID == 0 {
			t.Errorf("event %d got id 0", i)
		}
	}
	got, err := s.ListAuditEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d events, want 3", len(got))
	}
	// Newest first; the last write should be on top.
	if got[0].Action != "cost.zero_out" {
		t.Errorf("got[0].Action = %q, want cost.zero_out (newest first)", got[0].Action)
	}
}

// QUB-112: audit events reject empty actor and action.
func TestRecordAuditEvent_RejectsEmptyFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.RecordAuditEvent(ctx, AuditEvent{Action: "rerun.create"}); err == nil {
		t.Error("empty actor accepted; should reject")
	}
	if _, err := s.RecordAuditEvent(ctx, AuditEvent{Actor: "x"}); err == nil {
		t.Error("empty action accepted; should reject")
	}
}

// QUB-112: retention schedule. Every run gets a
// scheduled-deletion timestamp; the dashboard renders
// it on the run-detail page.
func TestListRetentionSchedule_ComputesCutoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, started)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.ListRetentionSchedule(ctx, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	wantDel := started.Add(90 * 24 * time.Hour)
	if !got[0].ScheduledDeletion.Equal(wantDel) {
		t.Errorf("scheduled_deletion = %v, want %v", got[0].ScheduledDeletion, wantDel)
	}
}

// QUB-108: failure_class round-trip. The reconciler writes
// the K8s container exit reason into this column; the
// dashboard reads it for the exception dock's filter chips.
func TestUpsertRun_FailureClassRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(id, "a", "b", 1, "aaaaaaa", StatusFailed, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetRunFailureClass(ctx, id, "oom_killed"); err != nil {
		t.Fatalf("set failure class: %v", err)
	}
	got, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FailureClass != "oom_killed" {
		t.Errorf("failure_class = %q, want oom_killed", got.FailureClass)
	}
	// Clear and re-read.
	if err := s.SetRunFailureClass(ctx, id, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FailureClass != "" {
		t.Errorf("failure_class after clear = %q, want empty", got.FailureClass)
	}
}

// QUB-108: set on a missing run is a no-op (retention race).
func TestSetRunFailureClass_UnknownRun(t *testing.T) {
	s := newTestStore(t)
	err := s.SetRunFailureClass(context.Background(), "no-such-run", "oom_killed")
	if !errors.Is(err, ErrUnknownRun) {
		t.Errorf("err = %v, want ErrUnknownRun", err)
	}
}

// QUB-108: run_stages upsert is idempotent on (run_id, stage).
// The runner is at-least-once; a re-delivery of the same
// stage must not create a duplicate row.
func TestUpsertRunStage_IdempotentOnStage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(id, "a", "b", 1, "aaaaaaa", StatusRunning, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if _, err := s.UpsertRunStage(ctx, RunStage{
			RunID: id, Stage: "clone", StartedAt: now, Meta: `{"attempt":1}`,
		}); err != nil {
			t.Fatalf("upsert stage: %v", err)
		}
	}
	stages, err := s.ListRunStages(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stages) != 1 {
		t.Errorf("got %d stages, want 1 (re-deliveries must upsert)", len(stages))
	}
}

// QUB-108: heartbeat touch. The runner POSTs every 30s; the
// receiver stamps the server clock. A second call updates
// the field; an unknown run returns sql.ErrNoRows.
func TestTouchRunHeartbeat_UpdatesAndMissing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(id, "a", "b", 1, "aaaaaaa", StatusRunning, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.TouchRunHeartbeat(ctx, id); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastHeartbeatAt == nil {
		t.Fatalf("last_heartbeat_at not set")
	}
	if err := s.TouchRunHeartbeat(ctx, "missing"); !errors.Is(err, ErrUnknownRun) {
		t.Errorf("err = %v, want ErrUnknownRun", err)
	}
}

// QUB-108: installation controls. Pausing mutes webhooks;
// lens_opt_out is a JSON array. The GitHub poll must not
// overwrite an operator's pause.
func TestSetInstallationControls_AndPauseCheck(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ins := []Installation{
		{ID: 1, AccountLogin: "alpha", AccountType: "User", FetchedAt: time.Now().UTC()},
	}
	if err := s.UpsertInstallations(ctx, ins); err != nil {
		t.Fatalf("upsert installs: %v", err)
	}
	if err := s.SetInstallationControls(ctx, 1, true, []string{"security", "deep"}); err != nil {
		t.Fatalf("set controls: %v", err)
	}
	paused, err := s.IsInstallationPaused(ctx, 1)
	if err != nil {
		t.Fatalf("is paused: %v", err)
	}
	if !paused {
		t.Errorf("paused = false, want true")
	}
	got, err := s.GetInstallation(ctx, 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Paused {
		t.Errorf("paused field = false")
	}
	if len(got.LensOptOut) != 2 || got.LensOptOut[0] != "security" {
		t.Errorf("lens_opt_out = %v, want [security deep]", got.LensOptOut)
	}
	// A subsequent UpsertInstallations from the GitHub poll
	// must not silently clear the operator's pause.
	if err := s.UpsertInstallations(ctx, ins); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	paused, err = s.IsInstallationPaused(ctx, 1)
	if err != nil {
		t.Fatalf("is paused after re-upsert: %v", err)
	}
	if !paused {
		t.Errorf("paused cleared by GitHub poll — operator mute lost")
	}
}

// QUB-108: refund audit row. Every "zero out cost" action
// appends a row; no UPDATE path.
func TestRecordRefund_AppendOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(id, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for i, tokens := range []int64{100, 200, 50} {
		if _, err := s.RecordRefund(ctx, Refund{
			RunID:      id,
			Lens:       "deep",
			Tokens:     tokens,
			RefundedBy: "tester",
		}); err != nil {
			t.Fatalf("refund %d: %v", i, err)
		}
	}
	got, err := s.ListRefunds(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d refunds, want 3 (append-only)", len(got))
	}
	// Newest first.
	if got[0].Tokens != 50 || got[2].Tokens != 100 {
		t.Errorf("ordering broken: got[0]=%d got[2]=%d", got[0].Tokens, got[2].Tokens)
	}
}

// QUB-109: lens telemetry is replaced atomically. The
// runner's at-least-once delivery is safe — a re-run
// lands on the same shape the dashboard expects.
func TestReplaceLensTelemetry_AtomicReplace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := "boop-a-b-1-aaaaaaa"
	if _, err := s.UpsertRun(ctx, sampleRun(id, "a", "b", 1, "aaaaaaa", StatusSucceeded, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	first := []LensTelemetry{
		{RunID: id, Lens: "security", CostUSD: 0.05, InputTokens: 100, OutputTokens: 50},
		{RunID: id, Lens: "deep", CostUSD: 0.20, InputTokens: 400, OutputTokens: 200},
	}
	if err := s.ReplaceLensTelemetry(ctx, id, first); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	// Re-run with a different cost profile.
	second := []LensTelemetry{
		{RunID: id, Lens: "security", CostUSD: 0.04, InputTokens: 80, OutputTokens: 40},
	}
	if err := s.ReplaceLensTelemetry(ctx, id, second); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	got, err := s.ListLensTelemetry(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d rows, want 1 (replace must be atomic, not additive)", len(got))
	}
	if got[0].Lens != "security" || got[0].CostUSD != 0.04 {
		t.Errorf("row = %+v, want security/$0.04", got[0])
	}
}

// QUB-109: lens cost rollup groups by lens across the time
// window. The dashboard's "lens is the row grain" rule.
func TestLensCostSummary_GroupsByLens(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	runs := []struct {
		id     string
		status RunStatus
		offset time.Duration
	}{
		{"boop-a-b-1-aaaaaaa", StatusSucceeded, -2 * time.Hour},
		{"boop-a-b-2-bbbbbbb", StatusSucceeded, -1 * time.Hour},
		{"boop-c-d-3-ccccccc", StatusFailed, -30 * time.Minute},
	}
	for _, r := range runs {
		if _, err := s.UpsertRun(ctx, sampleRun(r.id, "a", "b", 1, "aaaaaaa", r.status, now.Add(r.offset))); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	if err := s.ReplaceLensTelemetry(ctx, "boop-a-b-1-aaaaaaa", []LensTelemetry{
		{RunID: "boop-a-b-1-aaaaaaa", Lens: "security", CostUSD: 0.05},
		{RunID: "boop-a-b-1-aaaaaaa", Lens: "deep", CostUSD: 0.20},
	}); err != nil {
		t.Fatalf("lens tel 1: %v", err)
	}
	if err := s.ReplaceLensTelemetry(ctx, "boop-a-b-2-bbbbbbb", []LensTelemetry{
		{RunID: "boop-a-b-2-bbbbbbb", Lens: "deep", CostUSD: 0.10},
		{RunID: "boop-a-b-2-bbbbbbb", Lens: "security", CostUSD: 0.02},
	}); err != nil {
		t.Fatalf("lens tel 2: %v", err)
	}
	got, err := s.LensCostSummary(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (deep, security)", len(got))
	}
	// Ordered by cost desc: deep (0.30) > security (0.07).
	// Use a small tolerance — float addition is not
	// associative so 0.20 + 0.10 lands on
	// 0.30000000000000004 in IEEE 754.
	if got[0].Lens != "deep" || diff(got[0].CostUSD, 0.30) > 1e-6 {
		t.Errorf("row[0] = %+v, want deep/$0.30", got[0])
	}
	if got[1].Lens != "security" || diff(got[1].CostUSD, 0.07) > 1e-6 {
		t.Errorf("row[1] = %+v, want security/$0.07", got[1])
	}
}

// QUB-109: stuck-runs query selects running runs whose
// last_heartbeat_at is older than the threshold (or null).
// A succeeded/failed run is never "stuck" even if it has
// no heartbeat.
func TestListStuckRuns_FiltersByHeartbeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	runs := []struct {
		id     string
		status RunStatus
		hb     *time.Time // nil = never heartbeat'd; set = write directly
	}{
		{"boop-a-b-1-aaaaaaa", StatusRunning, nil},                              // stuck (no heartbeat)
		{"boop-a-b-2-bbbbbbb", StatusRunning, ptr(now.Add(-3 * time.Minute))},   // stuck (old)
		{"boop-a-b-3-ccccccc", StatusRunning, ptr(now.Add(-10 * time.Second))}, // healthy
		{"boop-a-b-4-ddddddd", StatusSucceeded, nil},                            // not stuck (terminal)
	}
	for _, r := range runs {
		if _, err := s.UpsertRun(ctx, sampleRun(r.id, "a", "b", 1, "aaaaaaa", r.status, now)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if r.hb != nil {
			// Write the heartbeat directly so the test
			// controls the timestamp (TouchRunHeartbeat
			// always stamps now, which would defeat the
			// "stale heartbeat" half of the assertion).
			if _, err := s.db.ExecContext(ctx, `UPDATE runs SET last_heartbeat_at = ? WHERE id = ?`,
				r.hb.UTC().Format(time.RFC3339Nano), r.id); err != nil {
				t.Fatalf("set hb: %v", err)
			}
		}
	}
	got, err := s.ListStuckRuns(ctx, 2*time.Minute, 50)
	if err != nil {
		t.Fatalf("list stuck: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d stuck, want 2 (the two running-without-recent-hb rows)", len(got))
	}
}
