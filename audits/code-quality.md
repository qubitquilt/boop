# Code Quality Audit
**Date:** 2026-08-06
**Scope:** Stacked PR `feature/qub-112-dashboard-cross-cutting` (5 commits, QUB-108/109/110/111/112 from main)
**Files reviewed (changed code only):**

- `apps/receiver/cmd/receiver/main.go` (+36)
- `apps/receiver/internal/dashboard/dashboard.go` (NEW, 497 lines)
- `apps/receiver/internal/dashboard/templates/*.html` (8 NEW, ~340 lines)
- `apps/receiver/internal/store/audit.go` (NEW, 200)
- `apps/receiver/internal/store/installations.go` (+224)
- `apps/receiver/internal/store/lens_telemetry.go` (NEW, 204)
- `apps/receiver/internal/store/migrations.go` (+329)
- `apps/receiver/internal/store/rerun.go` (NEW, 161)
- `apps/receiver/internal/store/runs.go` (+148/-37)
- `apps/receiver/internal/store/stages.go` (NEW, 301)
- `apps/receiver/internal/store/store_test.go` (+464)
- `apps/receiver/internal/webhook/dashboard.go` (+202 appended to existing 633)
- `apps/receiver/internal/webhook/handler.go` (+37 — `isPaused` + 2 call sites)
- `apps/receiver/internal/webhook/k8s_reconcile.go` (NEW, 257)
- `apps/receiver/internal/webhook/rerun.go` (NEW, 258)
- `apps/runner/src/index.mjs` (+27 — heartbeat, lens telemetry, dashboard-failure path)
- `apps/runner/src/lib/dashboard.mjs` (NEW, 225)
- `apps/runner/src/lib/dashboard.test.mjs` (NEW, 191)
- `apps/runner/src/lib/workflow.mjs` (+21 — stage POSTs)

---

## Summary

The store layer is the cleanest part of the PR — five new files (stages, lens_telemetry, rerun, audit, lens_telemetry) are well-shaped, well-commented, and covered by ~12 new `TestX` cases in `store_test.go`. The dashboard surface in `apps/receiver/internal/dashboard/` is the weakest part: the route table, the templates, and the Go view functions are mostly in sync, but a lineage-template field-name mismatch, a JSON-vs-form contract bug on the "Requeue" button, a form action that targets an unwired route, and a non-functional `failure_class` filter make the dashboard's "Requeue" and "Zero out cost" controls silently broken. Test coverage for the new dashboard code is thin: there are no tests for `RecordStage` / `RecordHeartbeat` / `RecordLensTelemetry` handlers, the entire `k8s_reconcile.go` module (including the `failureClassFromContainerState` taxonomy that the exception dock's filter chips depend on), or either `Rerun` / `RerunPreview` handler. The new code is honest about the gaps it doesn't ship (the `Rerun` response's "Phase 3 ships the lineage half" note, the unmigrated `webhook.hmac.*` audit action types) — that comment discipline is the single most useful pattern in the PR and is worth keeping in the next change.

**Highest-risk area:** the dashboard view layer (`apps/receiver/internal/dashboard/`) — the route table is missing three endpoints the templates reference, so two of the three exception-dock action buttons will return errors (or 404) the first time an operator clicks them. The next dashboard-touching change will land in a tangle of broken HTML contracts.

---

## Findings

### CQ-001
| Field    | Value |
|----------|-------|
| Tier     | 🔴 Blocking |
| Decide   | Change now |
| Location | `apps/receiver/internal/dashboard/templates/run_detail.html:55-67` references `.Lineage.Up` / `.Lineage.Down`, but `store.Lineage` (`apps/receiver/internal/store/rerun.go:99-102`) has fields `WalkUp` and `WalkDown` |

**Observation:** `serveRunDetail` calls `h.store.WalkLineage(...)` and binds the result as `data.Lineage`. The template iterates `.Lineage.Up` and `.Lineage.Down`, but the store struct's fields are `WalkUp` and `WalkDown`. Go's `html/template` is silent on missing fields (it renders the zero value), so the `<h3>Lineage</h3>` section will always render "No parent (this is the root of a chain)." even on a chain with three re-runs. The lineage data is being computed and stored, then thrown away by the template.

**Impact:** The re-run lineage feature is invisible from the dashboard. The QUB-110 schema (parent_run_id, superseded_by_id), the `WalkLineage` walk, the line-by-line audit test (`TestUpsertRun_LineageRoundTrip` at `store_test.go:744`), and the QUB-110 PR's note about "show me the lineage" all land correctly — but the operator-facing render is dead. The next click on the "Re-run" path will look broken even though the backend works.

**Suggestion:** Pick one name and stick to it. The store's `WalkUp` / `WalkDown` are clearer; change the template to `{{if .Lineage.WalkUp}}` and `{{range .Lineage.WalkUp}}` (4 sites in `run_detail.html`). A single one-line rename and the feature works.

---

### CQ-002
| Field    | Value |
|----------|-------|
| Tier     | 🔴 Blocking |
| Decide   | Change now |
| Location | `apps/receiver/internal/dashboard/templates/exceptions.html:30-37` — `Requeue` form posts to `/api/runs/{id}/rerun` with form-encoded body; `webhook/rerun.go:150-230` `Rerun` handler expects JSON |

**Observation:** The "Requeue" button is rendered as `<form method="post" action="/api/runs/{{.ID}}/rerun">` with `<input type="hidden" name="confirm" value="true">` and `<input type="hidden" name="reason" value="...">`. The form's Content-Type is `application/x-www-form-urlencoded`, but the `Rerun` handler does `json.NewDecoder(...).Decode(&body)` against the raw body — which fails on form-encoded data, returning 400 "bad json". The next click in the operator's browser surfaces a 400 to the dashboard with no useful error to the user.

**Impact:** The exception dock's primary re-queue action is broken in the most visible way — it returns an HTTP error the operator can read. The store's `Rerun` path is unreachable from the UI. This is exactly the kind of wiring bug that survives code review because each side is internally consistent (the form's spec is form-data; the handler's spec is JSON) and the inconsistency only shows on the first end-to-end click.

**Suggestion:** Pick one. Two options:
1. Change the handler to accept either `application/x-www-form-urlencoded` or JSON, parse conditionally, and return 400 only when neither matches. This is the smaller change and matches the pattern the operator is using (the dashboard's "Pause" form on `installations.html:18-22` is also form-encoded and goes through `serveInstallationControl`, so making the rerun endpoint accept form data is consistent with the existing dashboard pattern).
2. Change the form to a `<button>` that fires an `htmx` POST with a JSON body. This is the larger change and would require HTMX configuration in `layout.html`.

Option 1 is one helper in the rerun handler and a 3-line switch; option 2 is a refactor of the dashboard's form pattern. Recommend option 1.

---

### CQ-003
| Field    | Value |
|----------|-------|
| Tier     | 🔴 Blocking |
| Decide   | Change now |
| Location | `apps/receiver/internal/dashboard/templates/exceptions.html:35-37` — `Zero out cost` form posts to `/dashboard/runs/{id}/zero-cost`, but no such route exists in `dashboard.go:101-130` `route()` or in `main.go` |

**Observation:** The "Zero out cost" button is rendered as `<form method="post" action="/dashboard/runs/{{.ID}}/zero-cost">`. The dashboard's `route()` switch in `dashboard.go:101-130` handles `runs` and `runs/{id}` for GETs only (the middleware is mounted for both GET and POST at `dashboard.go:97-98`, but no `case` matches `runs/{id}/zero-cost`). The path falls through to `http.NotFound`. There is no `webhook/rerun.go` or `webhook/dashboard.go` handler that performs a zero-out either; the closest is the `refunds` table in `stages.go:197-234` and a `RecordRefund` method, but no HTTP handler wires a UI to it. The `audit_events.action` column's docstring (`audit.go:30-31`) and the migrations.go:439 comment both list `"cost.zero_out"` as a real audit action, but no caller emits it yet.

**Impact:** The button is wired in the UI but the backend isn't. The "no-op or 404" outcome is the lesser problem — the larger problem is that the operator clicks "Zero out cost" expecting the cost row to disappear from the dashboard, and instead the page returns 404. The "load-bearing first deliverable" claim in the migrations.go comment for QUB-108 ("OOMKilled runs light up on day one") is met, but the "zero out cost" follow-up is half-shipped.

**Suggestion:** Either remove the button (with a TODO comment that the action ships in a follow-up) or wire the route. Removing the button is a 3-line template change and matches the "Phase 3 ships the lineage half; K8s Job creation is wired in Phase 4" comment in `webhook/rerun.go:228`. Wiring the route is a follow-up that should add a `case` to `dashboard.go:route()` and a `zeroOutCost` handler that calls `h.store.RecordRefund(...)` and appends an `audit_events` row with `action: "cost.zero_out"`. Recommend removal for this PR — QUB-112 is the audit-log PR, and the audit log can be exercised end-to-end via the install pause/resume paths that *are* wired.

---

### CQ-004
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now |
| Location | `apps/receiver/internal/dashboard/dashboard.go:133-174` `serveRuns` reads `failure_class` from the query string but does not pass it to the store filter; `runs.html:14-20` shows a `failure_class` dropdown |

**Observation:** `serveRuns` parses `q.Get("failure_class")` (line 169) and passes it to the template as `Filter.FailureClass`, so the dropdown stays pre-selected on re-render. But the actual `store.ListRunsFilter` (line 135) doesn't include a `FailureClass` field, and the call at line 147 doesn't filter by it. The result: the operator selects `oom_killed` in the filter, the URL becomes `?failure_class=oom_killed`, the page reloads, and every run shows up — only the dropdown is preselected. The exception dock view (`serveExceptions` at line 356) does the filter correctly in Go (it pulls StatusFailed, then iterates in-memory to filter on `run.FailureClass`), so the operator can get the same data via a different route, but the "any class" path in `runs.html` is broken.

**Impact:** A filter the UI offers doesn't filter. This is the kind of bug operators learn to work around (they navigate to the exception dock instead), and the workaround erodes the trust in the runs view. The fix is also the dashboard's first step toward a real filter bar.

**Suggestion:** Add `FailureClass string` to `ListRunsFilter` and a `failure_class = ?` clause in `ListRuns`. Three changes: the struct, the SQL builder, and the dashboard handler. The store already indexes `failure_class` (`migrations.go:298`), so the query stays a single B-tree descent.

---

### CQ-005
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now |
| Location | `apps/receiver/internal/webhook/dashboard.go:484-529` `RecordStage`; `apps/receiver/internal/webhook/k8s_reconcile.go` entire file (257 lines); `apps/receiver/internal/webhook/rerun.go:150-230` `Rerun` and `:68-130` `RerunPreview` — no test coverage |

**Observation:** A search for `TestRecordStage | TestRecordHeartbeat | TestRecordLensTelemetry | TestRerun | TestRerunPreview | TestStartJobReconciler | TestClassifyJobExit` in `apps/receiver/` returns zero matches. The new HTTP handlers and the entire K8s reconciler module (including the `failureClassFromContainerState` taxonomy that the exception dock's filter chips depend on) ship without direct tests. The existing `dashboard_test.go` covers the older `RecordTelemetry` / `RecordStatus` / `ListRuns` / `Stats` paths and was extended to 14 cases for the new dashboard, but stops at the boundary of the new code.

**Impact:** The exception dock's promise — "OOMKilled runs light up on day one" — is enforced by `failureClassFromContainerState` at `k8s_reconcile.go:47-62`, which maps K8s container states to dashboard filter-chip strings. Every string the function returns (`oom_killed`, `container_error`, `crash_loop`, `image_pull`, `stuck_*`, `container_*`) is a dashboard filter chip in `exceptions.html:9-13` and `runs.html:16-19`. The function has six `case` branches across `mapExitReason` and `mapWaitingReason` and the only tests are the developer's mental model. A future change to the K8s API (a new container exit reason, a renamed `Waiting` cause) is the first thing the dashboard's exception dock will break on, and the test for it is the cutover. The same risk applies to `Rerun` / `RerunPreview` (the operator's "Re-run" path, gated by the same X-BOOP-Dashboard-Token as the rest of the dashboard — misconfiguring the JSON-body contract will surface in production the first time someone re-runs a run).

**Suggestion:** Add the missing tests. Sketches:
```go
// k8s_reconcile_test.go
func TestFailureClassFromContainerState_OOMKilled(t *testing.T) {
  cs := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}
  if got := failureClassFromContainerState(cs); got != "oom_killed" { t.Errorf("got %q", got) }
}
// One per case in mapExitReason + mapWaitingReason.

func TestClassifyJobExit_PicksLastTerminated(t *testing.T) {
  // Drive classifyJobExit with a stub h.kube that returns a Pod
  // whose LastTerminationState.Terminated.Reason = "OOMKilled".
  // Assert the returned class is "oom_killed".
}

func TestRerunPreview_HappyPath(t *testing.T) {
  h := newTestHandlerWithStore(t)
  // seed a run, GET /api/runs/{id}/rerun-preview, assert 200 + body shape.
}

func TestRerun_RequiresConfirm(t *testing.T) {
  h := newTestHandlerWithStore(t)
  rr := doJSON(t, h, "POST", "/api/runs/x/rerun", `{"reason":"x"}`, nil) // no confirm
  if rr.Code != 400 { t.Errorf("got %d, want 400", rr.Code) }
}

func TestRecordStage_HappyPath(t *testing.T) {
  h := newTestHandlerWithStore(t)
  rr := doRequest(t, h, "POST", "/api/runs/{id}/stages", `{"stage":"clone","ended":true}`, ...)
  if rr.Code != 204 { t.Errorf("got %d", rr.Code) }
}
```
The store already has the in-memory machinery (a real `Store` from `newTestHandlerWithStore` is used by the existing tests), so the test surface is small — the missing piece is just the table of inputs.

---

### CQ-006
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/receiver/internal/store/installations.go:55-151` `UpsertInstallations` (96 lines, CC ~12) — three concerns in one transaction |

**Observation:** `UpsertInstallations` performs three distinct jobs in one transaction: (1) read prior operator-controlled fields for the incoming ids (`:78-102`, the read-before-DELETE preservation dance the comment at `:48-54` calls out), (2) DELETE then INSERT in the same transaction (`:104-146`), (3) write the row using the resolved prior values (`:117-146`). The function is 96 lines, has 12+ branches, and the body is hard to scan because the prior-fields map's anonymous struct type is defined twice (`:68-71` and `:93-96`) and the loop body has a five-line `if` block for "do we have prior values for this id, or use defaults?". The logic is correct — `TestSetInstallationControls_AndPauseCheck` (`store_test.go:992-1033`) proves the operator's pause survives a GitHub poll — but the function is on the edge of the 50-line / 10-branch thresholds and the read-before-DELETE preservation is the kind of subtle code that a future "simplify this" refactor will break.

**Impact:** A future contributor lands on `UpsertInstallations` to fix a small bug (e.g. add a column to the INSERT) and the test suite passes, but the operator's pause toggle silently regresses because the read-before-DELETE was clobbered. The "fail-open" pattern is invisible without a comment — the comment is here, but the structural separation is not. The function is on the path of every GitHub poll (5-min cadence), so the cost of a refactor is low and the cost of a silent regression is operator mute.

**Suggestion:** Extract the read-before-DELETE block into a `readPriorControls(ctx, tx, ids) (map[int64]controls, error)` helper. The `controls` struct can be a named type (the current anonymous-struct-defined-twice pattern is a smell). The function then becomes `readPriorControls → DELETE → INSERT with prior-or-defaults` in three clear blocks. Defer to a follow-up PR because the code is correct and tested today.

---

### CQ-007
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now |
| Location | `apps/receiver/internal/dashboard/dashboard.go:474-478` `renderJSON` is unused; `:496-497` `var _ = context.Background` is an import-stabilization hack |

**Observation:** `renderJSON` is defined as a "debug helper used during development; not wired to a route but kept around" but no caller in the dashboard package or anywhere else in the repo references it. `var _ = context.Background` at the bottom of the file is a comment-tracked "ensure context import survives goimports" — the `context` import is required only because of that single line, which exists only because of the import. Two pieces of dead code, each other's reason to exist.

**Impact:** Small. The `renderJSON` function will be discovered by the next reader who greps for `JSON` and waste 5 minutes looking for a route that doesn't exist. The `var _` line is a comment-flagged footgun: a `goimports` run on a future file that uses `context` will silently drop the `_` and break nothing, but a future refactor that drops the `context` import will re-add the `_` to keep the import. The PR's "no new code without a route" norm is the right one to follow here.

**Suggestion:** Delete both. The `context` import is no longer needed after deletion (`grep "context\." dashboard.go` shows zero uses besides the dead `_` line); `goimports` will remove the import on the next save. If a JSON debug helper is ever wanted, the API surface already has `webhook.writeJSON` and the dashboard can wrap it.

---

### CQ-008
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/receiver/internal/dashboard/dashboard.go:101-130` `route` (CC ~9) + `dashboard.go:443-468` `serveInstallationControl` (CC ~5) — every new view adds another `case` to the same switch |

**Observation:** `route()` is a 30-line switch on `path` with nine cases; six are pure GETs that map to `h.serveX` methods, one is the redirect-to-runs landing, and two are the parameterized `runs/{id}` and `installations/{id}` patterns. The 9-case switch is on the edge of "a new view requires touching two places" (the switch and a new `serveY` method), and the `serveInstallationControl` POST handler at `:443-468` does the form parsing + lens-list parsing + store call + redirect inline. A new view ("retention schedule", "audit log") will require (a) adding a `case` to the switch, (b) adding a new `serveY` method, (c) writing a new template. The wiring is honest, but the surface area scales linearly with each view, and the file is already 497 lines.

**Impact:** Low for the next view (it's three mechanical additions), but the per-view cost will add up. The cleanest signal of this is `serveInstallationControl`: it has its own lens-CSV parsing logic inline (`:454-461`) that doesn't belong in a route handler — a future "lens editor" view (e.g., a per-lens enable toggle) will land in a third handler with a third copy of the same parsing.

**Suggestion:** Defer. The "small, ordered most-specific first" comment at `dashboard.go:108` is honest about the trade-off. When the next view is added, consider whether the route table is the right place for the parsing (it isn't) and whether a small `formParseLensOptOut(s string) []string` helper extracted from `serveInstallationControl` makes sense (it does). Not blocking; flag for the next dashboard PR.

---

### CQ-009
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/receiver/internal/webhook/rerun.go:74-89` and `webhook/dashboard.go` already-existing handlers — `buildJobName` is duplicated three times across the repo as `buildJobName` / `buildJobNameRerun` / `buildJobNamePrefix` |

**Observation:** Three "build a K8s Job name from (owner, repo, pr, sha7)" functions now exist: `handler.go`'s `buildJobName` (existing), `webhook/rerun.go:255-258` `buildJobNameRerun`, and `store/rerun.go:78-83` `buildJobNamePrefix`. The `shortSHARerun` helper at `webhook/rerun.go:246-251` is also a near-duplicate of the same helper in `handler.go`. The PR's own comments at `webhook/rerun.go:242-245` and `store/rerun.go:85-89` acknowledge the duplication: "Duplicated here because handler.go's are unexported and the re-run flow needs them. The implementations match exactly." The three regex sanitizers (`jobNameSanitizer` in `handler.go`, `rerunJobNameSanitizer` in `webhook/rerun.go:253`, `jobNameSanitizerRerun` in `store/rerun.go:89`) are three literals of the same `[^a-z0-9-]` pattern.

**Impact:** Three copies of the same string formatter. The comment is honest about the "can't import because unexported" reason, but a Job-name format change (e.g., adding a build-attempt suffix, switching to a longer short-SHA) now requires three coordinated changes with a silent failure mode if one is missed. The store copy is the most concerning because it lives behind a SQLite LIKE prefix match — a Job-name drift between the handler and the store would make `CountRerunJobsForSHA` return the wrong count without any error.

**Suggestion:** Leave as-is for this PR. A clean fix is a `internal/jobname` package with one function and one regex, but that's a refactor that touches `handler.go` and is outside QUB-110's scope. The PR comments name the exact split the next refactor should target.

---

### CQ-010
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/receiver/internal/dashboard/dashboard.go:142-146` — `q.Get("limit")` is read but explicitly dropped with `_ = s` |

**Observation:** The `serveRuns` handler reads `q.Get("limit")` then discards it with `_ = s` and a comment: "The default (50) is fine for a dashboard page; no override needed." This is honest dead code: the handler reads the parameter, doesn't pass it to `ListRunsFilter.Limit`, and the value falls on the floor. The `limit` parameter is not surfaced in the `runs.html` template's filter form (`:5-23`), so no user can actually set it. The `Limit` field on `ListRunsFilter` (store-side) is used by the JSON API (`webhook/dashboard.go:154-158` does pass it through).

**Impact:** Zero — the code is honest about what it does, the `_` is intentional, and the comment names the trade-off. The only cost is the future reader who greps for `limit` and wonders why it's there. A 30-second investigation resolves it.

**Suggestion:** Leave as-is. If a future dashboard view ever wants to pass a custom limit, the parameter is already wired. If not, deleting the block is a one-line cleanup. The honest comment is the right thing.

---

### CQ-011
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/dashboard.mjs:21-22` — `POST_TIMEOUT_MS = 5000` and `POST_RETRIES = 1` are module-level constants with no env override; `apps/receiver/internal/webhook/dashboard.go` `RecordStage` etc. have no timeout/Retry-After handling |

**Observation:** The runner's `postWithRetry` uses fixed 5s timeout and 1 retry with 200ms×attempt backoff. The receiver's `RecordStage` and `RecordHeartbeat` handlers do not set a `WriteTimeout` per-route — they share `WriteTimeout: 30 * time.Second` from `main.go:140`, which is the per-connection default. A receiver under load (multiple runners POSTing heartbeats and stages simultaneously) will hold connections open for up to 30s before the dashboard-side receiver closes them. The runner's 5s timeout will fire first, but the runner will retry once and the receiver will continue processing the original request — no correctness bug, just an asymmetry that will surface as "mysterious 5xx" if the receiver ever slows down.

**Impact:** Low — this is the kind of asymmetry that doesn't show up in tests but shows up in the first incident. The "best-effort, drop on failure" contract (per the file-level comment at `dashboard.mjs:1-17`) is the right one for the runner side, so the constants don't need to change. The receiver's per-route WriteTimeout is a separate, larger question (the existing telemetry/status handlers have the same property and the load tests presumably cover it).

**Suggestion:** Leave as-is. If a future PR adds a load test that hammers the receiver with concurrent heartbeats, the per-route WriteTimeout is a follow-up to consider, but it's not in this PR's scope.

---

## Metrics at a Glance

| File | Function | CC | LOC | Notable coupling |
|------|----------|----|-----|------------------|
| `dashboard/dashboard.go` | `serveRuns` | 5 | 41 | reads `failure_class` from query but doesn't filter on it (CQ-004) |
| `dashboard/dashboard.go` | `serveRunDetail` | 5 | 29 | calls `WalkLineage`; template references `.Lineage.Up` but struct has `.Lineage.WalkUp` (CQ-001) |
| `dashboard/dashboard.go` | `renderWaterfall` | 5 | 42 | pure (no I/O); the densest piece of dashboard logic |
| `dashboard/dashboard.go` | `route` | 9 | 30 | linear switch on path; every new view adds a case (CQ-008) |
| `dashboard/dashboard.go` | `serveInstallationControl` | 5 | 25 | inline form parsing of `lens_opt_out` CSV |
| `dashboard/dashboard.go` | `renderJSON` | n/a | 5 | **dead code** (CQ-007) |
| `dashboard/dashboard.go` | `var _ = context.Background` | n/a | 1 | **dead code** (CQ-007) |
| `store/stages.go` | `UpsertRunStage` | 4 | 30 | clean ON CONFLICT shape |
| `store/stages.go` | `ListStuckRuns` | 4 | 36 | LIMIT-clamp pattern repeats across the file (CQ-011 has related note) |
| `store/lens_telemetry.go` | `ReplaceLensTelemetry` | 5 | 41 | atomic DELETE+INSERT in transaction; tested at `store_test.go:1070` |
| `store/lens_telemetry.go` | `LensCostSummary` | 3 | 34 | clean GROUP BY query |
| `store/audit.go` | `RecordAuditEvent` | 4 | 26 | rejects empty actor/action; tested at `store_test.go:861` |
| `store/audit.go` | `ListRetentionSchedule` | 5 | 36 | computes scheduled_deletion in Go, not SQL |
| `store/rerun.go` | `WalkLineage` | 5 | 45 | bounded walk with cycle defense |
| `store/installations.go` | `UpsertInstallations` | 12 | 96 | read-prior → DELETE → INSERT in one tx (CQ-006) |
| `webhook/dashboard.go` | `RecordStage` | 7 | 45 | **no test** (CQ-005) |
| `webhook/dashboard.go` | `RecordHeartbeat` | 5 | 25 | **no test** (CQ-005) |
| `webhook/dashboard.go` | `RecordLensTelemetry` | 6 | 45 | **no test** (CQ-005) |
| `webhook/dashboard.go` | `ListRuns` | 9 | 58 | seven optional query params; tested indirectly via the dashboard view |
| `webhook/dashboard.go` | `Stats` | 9 | 69 | four sub-queries; tested at `dashboard_test.go:129` |
| `webhook/rerun.go` | `RerunPreview` | 9 | 62 | **no test** (CQ-005) |
| `webhook/rerun.go` | `Rerun` | 11 | 80 | **no test**; CC crosses 10 (CQ-005) |
| `webhook/k8s_reconcile.go` | `StartJobReconciler` | 5 | 28 | goroutine + cancel + interval clamp |
| `webhook/k8s_reconcile.go` | `reconcileJobsOnce` | 3 | 25 | K8s API + store backfill |
| `webhook/k8s_reconcile.go` | `classifyJobExit` | 4 | 44 | **no test** (CQ-005) |
| `webhook/k8s_reconcile.go` | `failureClassFromContainerState` | 3 | 16 | **no test** (CQ-005) |
| `webhook/k8s_reconcile.go` | `mapExitReason` + `mapWaitingReason` | 5+5 | 24+15 | **no test** (CQ-005) |
| `runner/src/lib/dashboard.mjs` | `postWithRetry` | 5 | 30 | single retry on 5xx/timeout; 4xx/202 dropped |
| `runner/src/lib/dashboard.mjs` | `startHeartbeat` | 4 | 29 | unref'd setInterval; returns idempotent stop |
| `runner/src/lib/workflow.mjs` | `runStages` | 8 | 56 | post-start + try/finally post-end wrapping |
| `runner/src/lib/workflow.mjs` | `withRetry` | 8 | 68 | exponential backoff inside stage loop |
| `runner/src/index.mjs` | `onStagePassed` | 7 | 42 | QUB-92 resume merge + writeWorkflowState |
| `apps/runner/src/lib/dashboard.mjs` | test surface | n/a | 191 | 14 tests, all happy-path + retry/reject |

---

## Unable to Verify

- **`RecordStage`'s `DurationMS = 0` placeholder on the end POST (`webhook/dashboard.go:519-521`).** The code sets `DurationMS = 0` for the end POST because the runner's own clock isn't authoritative, and the comment claims the waterfall will compute the duration at render time via `durMS(s)`. The render path at `dashboard.go:297-305` confirms the fallback: `if s.EndedAt != nil { return s.EndedAt.Sub(s.StartedAt).Milliseconds() }` computes the duration from the server-stamped timestamps. So the `0` is harmless. But a future change that treats `DurationMS = 0` as "missing" would break the end-POST contract; worth a one-line comment that says "always 0; the render computes the real value" rather than the current comment about "<1s" rendering.
- **The 11 `cc` on `Rerun` (`webhook/rerun.go:150-230`).** I counted 11 manually; the function has 80 lines and a linear flow with one if-chain (confirm, reason, status check, count, name, upsert, supersede, log, writeJSON). The single retryable error path is the upsert failure. The function is correct and readable but on the edge of the 10-branch threshold. A future change that adds a second error-mapping branch (e.g., translating the store's `ErrDuplicateKey` into a 409) will push it over. Worth a small refactor when the K8s Job creation (the "Phase 4 wires the K8s jobbuilder half" follow-up) lands, because the Job-creation path will add its own error-mapping branch.
- **The `var _ = context.Background` line (`dashboard.go:496-497`).** Confirmed unused in the rest of the file via grep; deletion is safe.
- **The new test at `store_test.go:1070` `TestReplaceLensTelemetry_AtomicReplace`.** The "atomic" claim is enforced by the test's setup (one Replace, then another), but the test does not exercise a partial-failure path (e.g., a network drop in the middle of the transaction). SQLite's BEGIN/COMMIT is ACID at the SQL level; the test confirms the application behavior. Not a gap to fix — the SQL-level atomicity is the store's contract.
- **The `webhook.hmac.fail` and `webhook.hmac.pass` audit action types** listed in `migrations.go:441-442` as "cross-cutting ledger" but not referenced in any handler. The docstring at `audit.go:5-17` calls the audit log "QUB-112" and lists the actions the dashboard emits — neither `webhook.hmac.*` action is in the dashboard's vocabulary. This is a forward-reference to a different PR (the HMAC ledger the audit log was originally scoped alongside) that hasn't landed. Not blocking; flag for the next audit-touching change.
- **The `_ = s` discard on `q.Get("limit")` in `dashboard.go:142-146`.** Confirmed via grep; the parameter is read but not used. CQ-010.
