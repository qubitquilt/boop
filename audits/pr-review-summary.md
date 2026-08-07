# PR Review: feat(receiver): boop admin dashboard cross-cutting (QUB-108 / 109 / 110 / 111 / 112)

**Date:** 2026-08-06
**Branch:** `feature/qub-112-dashboard-cross-cutting`
**Stacked commits:** 5 (QUB-108 → QUB-112, all on top of `main`)
**Files reviewed:** 26 files, **+3946 / −46**

---

## Executive Summary

This is a five-commit, additive cross-cutting that turns the receiver into an operator-facing dashboard. The store layer is the strongest part: well-shaped migrations (v1→v5), well-commented Go APIs, ~12 new store-level tests, and a clean `UNIQUE(run_id, stage)` + `COALESCE(excluded, existing)` idiom for at-least-once stage POSTs. The new HTTP handlers and templates are mostly in sync, but **the dashboard view layer ships with at least three buttons that don't work on first click**: the re-run lineage render references field names the struct doesn't expose (`.Lineage.Up` vs `WalkUp`), the "Requeue" form posts form-encoded data to a JSON-only handler (400), and the "Zero out cost" button targets an unwired route. On the data side, the new `RecordStage` handler hardcodes `duration_ms = 0` on every end POST and overwrites the real duration via the `ON CONFLICT` clause, and the same handler returns 500 (instead of 202) when the run row hasn't been committed yet — the waterfall silently degrades for any run that starts a stage before the receiver's `UpsertRun` lands. The audit log is fully built (schema, store API, tests) but no mutation path writes an event; QUB-112 ships a table that compliance review will flag as "no actor, no UI, no entry point."

**Highest-impact area:** the dashboard view layer + the new POST handlers. Six 🔴 findings, three of which are silent data loss / corruption in the user-facing waterfall.

**Merge-readiness signal: needs changes first.** Block on the six 🔴 items below; the rest are reasonable to ship as follow-ups.

---

## Priority Issue Table

| ID         | Tier        | File : Line                                                          | Summary                                                                                    | Decide       |
|------------|-------------|----------------------------------------------------------------------|--------------------------------------------------------------------------------------------|--------------|
| EH-003     | 🔴 Blocking | `apps/receiver/internal/webhook/dashboard.go:519-521`                | `RecordStage` hardcodes `duration_ms = 0`; overwrites real duration via `ON CONFLICT` clause | Change now   |
| EH-001     | 🔴 Blocking | `apps/receiver/internal/webhook/dashboard.go:484-529`                | `RecordStage` returns 500 on FK violation for not-yet-persisted run; should return 202       | Change now   |
| EH-002     | 🔴 Blocking | `apps/receiver/internal/webhook/dashboard.go:611-656`                | `RecordLensTelemetry` same gap; `ReplaceLensTelemetry` 500s and retry drops the cost row     | Change now   |
| CQ-001 / DP-001 | 🔴 Blocking | `apps/receiver/internal/dashboard/templates/run_detail.html:55-66` | Template reads `.Lineage.Up`/`.Down`; struct has `WalkUp`/`WalkDown`. Re-run lineage UI is silently empty | Change now   |
| CQ-002     | 🔴 Blocking | `apps/receiver/internal/dashboard/templates/exceptions.html:30-34`  | "Requeue" form posts `application/x-www-form-urlencoded`; `Rerun` handler parses JSON. First click → 400 | Change now   |
| CQ-003 / DP-003 / SP-002 | 🔴 Blocking | `apps/receiver/internal/dashboard/templates/exceptions.html:35-37` + `webhook/rerun.go:135-136` | "Zero out cost" form posts to `/dashboard/runs/{id}/zero-cost` (no route). `Rerun` docstring claims audit log written — it isn't. | Change now   |
| CQ-004 / SP-001 | 🔴 Blocking | `apps/receiver/internal/dashboard/dashboard.go:135-141` + `store/runs.go:255-264` | `serveRuns` reads `failure_class` query param but never forwards it to the store; dropdown is a no-op | Change now   |
| EH-006     | 🟡 Follow-up | `webhook/rerun.go:135-136`, `dashboard/dashboard.go:443-468`      | Audit log has zero producers; `Rerun` and `serveInstallationControl` don't call `RecordAuditEvent` | Change now   |
| EH-005     | 🟡 Follow-up | `webhook/rerun.go:194-218`                                          | `Rerun` writes the new row + `superseded_by_id` in two separate transactions               | Change now   |
| EH-004     | 🟡 Follow-up | `apps/runner/src/lib/workflow.mjs:314`                              | `await postStage(...)` on start POST contradicts the helper's "fire-and-forget" contract; +10s × stages on a degraded receiver | Change now   |
| CQ-005     | 🟡 Follow-up | `webhook/dashboard.go:484-529, 611-656`, `webhook/k8s_reconcile.go`, `webhook/rerun.go:150-230, 68-130` | Zero direct tests for `RecordStage`/`RecordHeartbeat`/`RecordLensTelemetry`, the entire K8s reconciler (including the `failureClassFromContainerState` taxonomy), or `Rerun`/`RerunPreview` | Change now   |
| CQ-006     | 🟡 Follow-up | `store/installations.go:55-151`                                     | `UpsertInstallations` 96 lines, CC ~12; read-before-DELETE preservation dance is fragile    | Defer        |
| CQ-007     | 🟡 Follow-up | `dashboard/dashboard.go:474-478, 496-497`                           | `renderJSON` is dead code; `var _ = context.Background` is an import-keeper hack            | Change now   |
| EH-008     | 🟡 Follow-up | `webhook/rerun.go:194-218`                                          | `CountRerunJobsForSHA` → `UpsertRun` TOCTOU race; concurrent re-runs clobber each other      | Defer        |
| EH-009     | 🟡 Follow-up | `webhook/rerun.go:224-229`                                          | `Rerun` returns 202 + permanent `note` field; API contract drift                            | Defer        |
| EH-010     | 🟡 Follow-up | `webhook/dashboard.go:51-108`                                       | `ListInstallations` cold-start refresh silently swallows `UpsertInstallations` error        | Defer        |
| EH-007     | 🟡 Follow-up | `runner/src/lib/dashboard.mjs:212-220`                              | `postWithRetry` collapses 401 into "post rejected"; token-misconfig is invisible             | Defer        |
| RD-001     | 🟡 Follow-up | `store/audit.go:18-24, 195-200`                                     | `var _ sql.NullString` placeholder; entire `database/sql` import held up only by this line   | Change now   |
| RD-002 / DP-002 / CQ-009 / SP-003 | 🟡 Follow-up | `webhook/handler.go:1143-1151`, `webhook/rerun.go:242-257`, `store/rerun.go:78-89` | Job-name convention `boop-{owner}-{repo}-{pr}-{sha7}` held in three packages                | Defer        |
| RD-003     | 🟡 Follow-up | `webhook/dashboard.go:345, 407, 557`                                | "Unknown run" detection uses three shapes at the HTTP boundary (`ErrUnknownRun` vs raw `sql.ErrNoRows`) | Defer        |
| SP-004 / DP-005 | 🟡 Follow-up | `webhook/dashboard.go:428-438` vs `runner/src/lib/workflow.mjs:82-156` | Stage vocabulary duplicated by convention; live view filters `StatusRunning` and misses `auth`/`clone`/`review` | Defer        |
| SP-005 / SP-010 | 🟡 Follow-up | `webhook/handler.go:135-173`                                        | `*Handler` is now app container (webhook + 4 background loops + 12 dashboard endpoints); no store interface seam | Defer        |
| SP-006 / DP-009 | 🟡 Follow-up | `webhook/dashboard.go:167-177`, `dashboard/dashboard.go:154-164`   | N+1 telemetry query in `ListRuns` (HTML + JSON)                                            | Defer        |
| SP-008     | 🟡 Follow-up | `store/audit.go:145-181` + `templates/`                              | `ListRetentionSchedule` defined and tested; no dashboard route, no template                  | Change now (small) |
| DP-004     | 🟡 Follow-up | `webhook/dashboard.go:188-191`, `dashboard/dashboard.go:186-192` etc. | `store.Run` embedded directly in view structs; new column leaks to the wire                  | Defer        |
| DP-006     | 🟡 Follow-up | `dashboard/dashboard.go` (497 lines)                                | One file with 6 view handlers, route table, waterfall math, auth middleware, dead `renderJSON` | Defer        |
| DP-007     | 🟡 Follow-up | `dashboard/dashboard.go:195-224`                                    | `serveRunDetail` does 4 separate store reads; no transactional boundary                     | Defer        |
| CQ-008     | 🟢 Optional | `dashboard/dashboard.go:101-130, 443-468`                           | `route()` switch grows linearly; per-view form parsing (e.g. `lens_opt_out` CSV) is inlined  | Defer        |
| SP-007     | 🟢 Optional | `runner/src/lib/workflow.mjs:279`                                    | `deps.postStage || defaultPostStage` override path is dead; `overrides.postStage` never reaches this line | Leave as-is  |
| SP-009     | 🟢 Optional | `runner/src/index.mjs:46-52, 280-360`                                | `postStatus` is shadowed by `postDashboardStatus` import alias; dual-posting duplicated     | Leave as-is  |

**Counts:** 🔴 Blocking 8 · 🟡 Follow-up 19 (10 change-now + 9 defer) · 🟢 Optional 3.
**Top 3 for the author to act on first:** EH-003 (every stage duration is wrong), EH-001+EH-002 (silent data loss on waterfall + cost rollup), CQ-001 / DP-001 (lineage UI shows nothing).

---

## Categorized Findings

### Code Quality (CQ-*)

The store layer is the cleanest part of the PR — five new files, ~1,200 lines, well-commented, with ~12 new test cases in `store_test.go`. The dashboard view layer is the weakest: a few small template/handler contracts landed out of sync, and three of the new handler modules (`RecordStage`, `Rerun`, the K8s reconciler) ship with no direct test coverage. The honest docstring discipline ("Phase 3 ships the lineage half; K8s Job creation is wired in Phase 4") is the most useful pattern in the PR and is worth preserving.

The blocking CQ-001 → CQ-004 are all "template and Go struct disagreed on names" / "form and handler disagreed on Content-Type" / "button targeted an unwired route" — small, mechanical, and 1-3 line fixes each.

### Structural Choices (DP-*)

The structural pattern that works well: the store layer is the source of truth, the webhook/dashboard packages translate to HTTP, the runner POSTs best-effort to the receiver. The structural pattern that needs attention: the `webhook` package is now the de-facto app container — it owns the K8s reconciler, three background loops, the dashboard's GET/POST API, and the webhook handler. The store has no interface seam (only `ghClientAPI` does). The dashboard's view types embed `store.Run` directly, so every new column on the run table is implicitly shipped to the operator UI.

The DP-001 / DP-003 blocking findings (lineage field name, audit log producers missing) overlap with the CQ / EH blocking findings above.

### Error Handling (EH-*)

The dashboard data layer is structurally sound: HTTP status codes are mostly right (200/202/204/4xx/5xx), the runner's POST helpers use the at-least-once `UNIQUE(run_id, stage)` constraint correctly, and the receiver's foreign-key cascade keeps the orphan-row surface low. **The two highest-risk gaps are EH-001/002 (missing 202 fallback on `RecordStage`/`RecordLensTelemetry` for a not-yet-persisted run) and EH-003 (`RecordStage` hardcodes `duration_ms = 0` on every end POST, overwriting the real duration via `COALESCE`).** Both cause silent data loss / corruption in the user-facing waterfall.

The audit log gap (EH-006) is the contract gap that compliance review will catch — the table is built, the API is built, the tests are passing, and no production handler writes a row.

### Readability (RD-*)

Two patterns will slow the next reader down: (1) the new `webhook/rerun.go` re-declares `shortSHARerun` / `buildJobNameRerun` / `rerunJobNameSanitizer` to mirror unexported `handler.go` helpers, so the Job-name convention is now load-bearing knowledge held in two files; (2) the "unknown run" detection at the HTTP boundary uses three different shapes in two files (`store.ErrUnknownRun` in `RecordTelemetry` and the re-run handlers, but raw `sql.ErrNoRows` in `RecordStatus` and `RecordHeartbeat`). The `var _ sql.NullString` placeholder at the bottom of `store/audit.go` is a third one-line issue that should die before it gets copy-pasted.

### Structural & Dependency (SP-*)

The cross-cutting is shaped well for the rollback: each QUB-N commit introduces one new file or one narrow edit, and the data-layer changes in `store/` are additive migrations. The dashboard's own coupling risk is that the operator UI is built directly on `*store.Store` with no seam, and the receiver's `*Handler` is the de-facto app container for everything. The audit log and retention schedule (QUB-112) are the most worrying: the store-side API exists, the tests exist, and no caller writes a row or renders a view.

The runner's "fire-and-forget" docstring on `postStage` is contradicted by `await` at `workflow.mjs:314` (also EH-004) — the per-stage latency on a degraded receiver is 10s × N stages longer than the design says.

---

## Suggested PR Comments

### Comment 1 — Lineage view is silently empty

**File:** `apps/receiver/internal/dashboard/templates/run_detail.html:55-66` | **Lines:** 55–66
**Observation:** The template iterates `{{if .Lineage.Up}}` and `{{range .Lineage.Up}}`, but `store.Lineage` (`apps/receiver/internal/store/rerun.go:99-102`) has fields `WalkUp` and `WalkDown`. Go's `html/template` silently renders the zero value on missing fields, so the "Lineage" section always reads "No parent (this is the root of a chain.)" — even on a chain with three re-runs. The data is computed and stored; the template drops it.
**Impact:** The QUB-110 re-run lineage feature is invisible from the dashboard. The store round-trip test passes because it asserts the Go fields, not the template.
**Suggestion:**
```html
{{if .Lineage.WalkUp}}
<p>Walked {{len .Lineage.WalkUp}} ancestor(s):</p>
<ul>
  {{range .Lineage.WalkUp}}<li><a href="/dashboard/runs/{{.ID}}">{{.ID}}</a> — {{.Status}}</li>{{end}}
</ul>
{{else}}
<div class="empty">No parent (this is the root of a chain).</div>
{{end}}
{{if .Lineage.WalkDown}}
<h4>Superseded by</h4>
<ul>
  {{range .Lineage.WalkDown}}<li><a href="/dashboard/runs/{{.ID}}">{{.ID}}</a> — {{.Status}}</li>{{end}}
</ul>
{{end}}
```
*Decide: Change now*

### Comment 2 — `RecordStage` overwrites the real duration with 0

**File:** `apps/receiver/internal/webhook/dashboard.go:519-521` | **Lines:** 519–521
**Observation:** When `body.Ended` is true, the handler sets `dur := int64(0)` and passes it to `UpsertRunStage`. The `ON CONFLICT(run_id, stage) DO UPDATE SET duration_ms = COALESCE(excluded.duration_ms, run_stages.duration_ms)` clause takes `0` (not NULL) and overwrites the real duration. The dashboard's `durMS` (`dashboard.go:297-305`) returns 0 because `DurationMS` is non-nil, never falling through to the `EndedAt - StartedAt` fallback. Every waterfall bar's duration is wrong for stages longer than 1 second.
**Impact:** Operators have no signal that a stage was slow. The waterfall looks fine; the run actually took minutes on the slow stage.
**Suggestion:** Don't set `DurationMS` from the handler; let the dashboard compute it from `EndedAt - StartedAt`:
```go
if body.Ended {
    stage.EndedAt = &now
    // leave DurationMS nil — durMS() in the dashboard
    // computes the real value from EndedAt - StartedAt.
}
```
*Decide: Change now*

### Comment 3 — `RecordStage` and `RecordLensTelemetry` lose data on a not-yet-persisted run

**File:** `apps/receiver/internal/webhook/dashboard.go:484-529, 611-656` | **Lines:** 484–529, 611–656
**Observation:** Both handlers map every store error to 500 + body "store error". The schema has `run_id ... REFERENCES runs(id) ON DELETE CASCADE` (`migrations.go:235`) and `foreign_keys=on`, so a POST for a run the receiver hasn't committed yet fails the INSERT with FK violation. The runner's `postWithRetry` retries 5xx once, then drops the call. `RecordStatus` and `RecordHeartbeat` already handle the same race with `sql.ErrNoRows → 202 Accepted`; these two are the only POST endpoints that don't.
**Impact:** Silent data loss on the waterfall (start POSTs lost; end POSTs land and stamp `started_at = now`) and on the cost rollup (lens telemetry rows dropped) for any run that starts a stage before the receiver's `UpsertRun` is visible.
**Suggestion:** Mirror the `RecordStatus` pattern:
```go
if _, err := h.store.UpsertRunStage(r.Context(), stage); err != nil {
    if errors.Is(err, sql.ErrNoRows) {
        w.WriteHeader(http.StatusAccepted)
        return
    }
    h.logger.Warn("record stage", "run", id, "stage", body.Stage, "err", err)
    http.Error(w, "store error", http.StatusInternalServerError)
    return
}
```
Same fix for `RecordLensTelemetry` — pre-check with `GetRun` or interpret the FK error as "run not yet visible" and return 202.
*Decide: Change now*

### Comment 4 — "Zero out cost" button is wired to a route that doesn't exist

**File:** `apps/receiver/internal/dashboard/templates/exceptions.html:35-37` | **Lines:** 35–37
**Observation:** The "Zero out cost" button posts to `/dashboard/runs/{id}/zero-cost`. The dashboard's `route()` switch (`dashboard.go:101-130`) doesn't match this path; the closest is `RecordRefund` (`store/stages.go:197-234`), but no HTTP handler is wired. The `audit_events.action` docstring lists `"cost.zero_out"` as a real action, but no caller emits it. The same gap is in `Rerun`: the docstring (`webhook/rerun.go:135-136`) says the `reason` is "logged in the audit trail," but the handler never calls `RecordAuditEvent`.
**Impact:** The button returns 404 to the operator. The audit log's "actor is the load-bearing column" promise is empty — no producer writes a row. Compliance review will flag this.
**Suggestion:** Either remove the button (with a TODO comment that ships in a follow-up — matches the "Phase 3 ships the lineage half" pattern in `Rerun`), or wire both the route and the audit producer. Recommend both: add `serveZeroOutCost` to the dashboard, have it call `RecordRefund` + `RecordAuditEvent`, and add a `/dashboard/audit` view that uses the existing `ListAuditEvents` method.
*Decide: Change now*

### Comment 5 — "Requeue" form sends form-encoded data to a JSON-only handler

**File:** `apps/receiver/internal/dashboard/templates/exceptions.html:30-34` | **Lines:** 30–34
**Observation:** The "Requeue" form is rendered as `<form method="post" action="/api/runs/{{.ID}}/rerun">` with hidden `confirm` / `reason` inputs (Content-Type `application/x-www-form-urlencoded`). The `Rerun` handler does `json.NewDecoder(...).Decode(&body)` against the raw body, which fails on form-encoded data, returning 400 "bad json."
**Impact:** The exception dock's primary re-queue action is broken in the most visible way — it returns an HTTP error the operator can read. The store's `Rerun` path is unreachable from the UI.
**Suggestion:** Smaller fix: change `Rerun` to accept either `application/x-www-form-urlencoded` or JSON, parse conditionally, and return 400 only when neither matches. Consistent with the existing `serveInstallationControl` form pattern. One helper, a 3-line switch.
*Decide: Change now*

---

## What's Working Well

1. **Store migrations are well-shaped.** `migrations.go:51-155` runs each block idempotently with `IF NOT EXISTS` / `hasColumn` guards, writes `PRAGMA user_version` after each successful block, and resumes cleanly on a partial upgrade. The `v2 → v5` sequence adds the dashboard's hot tables (`run_stages`, `lens_telemetry`, lineage columns, `audit_events`) without breaking v1 (`runs`, `telemetry`, `installations`). The forward-comments on each block name the index target and the dashboard query that consumes it — `apps/receiver/internal/store/migrations.go:43-50`.

2. **`UpsertRunStage` enforces the at-least-once contract in SQL.** The `UNIQUE(run_id, stage)` constraint plus `ON CONFLICT(run_id, stage) DO UPDATE SET ended_at = COALESCE(excluded.ended_at, run_stages.ended_at), duration_ms = COALESCE(excluded.duration_ms, run_stages.duration_ms)` clause is the load-bearing correctness rule for the dashboard's waterfall. A re-delivered end POST that lands after the row is already closed cannot accidentally re-open a closed stage. A re-delivered start POST cannot clobber a closed stage's `ended_at`. This is the right shape: idempotency in the SQL, not the application — `apps/receiver/internal/store/stages.go:61-71`.

3. **`k8s_reconcile.go` keeps the K8s surface contained.** `failureClassFromContainerState` / `mapExitReason` / `mapWaitingReason` are a tiny, named, single-purpose trio that reads as a vocabulary mapping rather than a flow. The K8s-side `Waiting.Reason` strings (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`) are converted at the receiver boundary into the dashboard's filter-chip values (`oom_killed`, `container_error`, `crash_loop`, `image_pull`, `config_error`, `stuck_*`, `container_*`). Future taxonomy changes touch one function — `apps/receiver/internal/webhook/k8s_reconcile.go:47-103`.

4. **The `BOOP_DASHBOARD_TOKEN` gate is honest about being opt-in.** `dashboard/dashboard.go:76-92` rejects every request with 401 when the env var is empty, using `subtle.ConstantTimeCompare`. The `main.go:123-126` log line ("BOOP_DASHBOARD_TOKEN is unset: /dashboard/* will return 401") tells the operator exactly what state the receiver is in. The "data layer disabled" 503 path in every handler is consistent.

5. **The runner's `deps` bundle is clean.** Every lib function reads only what it needs from `deps`; tests inject `fetchImpl`, `spawnFn`, `log`, `errlog` per concern. The 14 tests in `dashboard.test.mjs` cover the no-op / 5xx-retry / 4xx-no-retry / 202-no-retry / batch shape / heartbeat contract — `apps/runner/src/lib/dashboard.test.mjs:1-191`.
