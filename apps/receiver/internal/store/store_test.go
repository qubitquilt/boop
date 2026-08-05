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

func TestRecordTelemetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.UpsertRun(ctx, sampleRun("boop-a-b-1-aaaaaaa", "a", "b", 1, "aaaaaaa", StatusSucceeded, now)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	telem := Telemetry{
		RunID:            "boop-a-b-1-aaaaaaa",
		Model:            "openrouter/anthropic/claude-3.5-sonnet",
		Provider:         "openrouter",
		InputTokens:      1000,
		OutputTokens:     500,
		ReasoningTokens:  0,
		CacheReadTokens:  200,
		CacheWriteTokens: 0,
		CostUSD:          0.0123,
		StepCount:        3,
		RecordedAt:       now,
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
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("tokens = %+v", got)
	}
	if got.CostUSD != 0.0123 {
		t.Errorf("cost = %f", got.CostUSD)
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
