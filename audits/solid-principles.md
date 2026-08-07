# Structural & Dependency Audit
**Date:** 2026-08-06
**Scope:** QUB-108 → QUB-112 dashboard cross-cutting. Branch `feature/qub-112-dashboard-cross-cutting`, 5 stacked commits. Files: `apps/receiver/internal/dashboard/dashboard.go` (new, 497 lines), `apps/receiver/internal/dashboard/templates/*.html` (7 new), `apps/receiver/internal/webhook/dashboard.go` (new, 833 lines), `apps/receiver/internal/webhook/rerun.go` (new, 258 lines), `apps/receiver/internal/webhook/k8s_reconcile.go` (new, 257 lines), `apps/receiver/internal/webhook/handler.go` (+37 lines: `isPaused` and call sites), `apps/receiver/internal/store/audit.go` (new, 200 lines), `apps/receiver/internal/store/rerun.go` (new, 161 lines), `apps/receiver/internal/store/migrations.go` (now v1–v5), `apps/runner/src/lib/dashboard.mjs` (new, 225 lines), `apps/runner/src/lib/workflow.mjs` (+21 lines), `apps/runner/src/index.mjs` (+28 lines for dashboard hooks).

---

## Summary

The cross-cutting is shaped well for the rollback: each QUB-N commit introduces one new file or one narrow edit, and the data-layer changes in `store/` are additive migrations. The dashboard's own coupling risk is that the operator UI is built directly on `*store.Store` with no seam, and the receiver's `*Handler` is now the de-facto app container for webhook + dashboard API + K8s reconciler + retention + backup + installation poller, with one struct and one `cfg`. The audit log and retention schedule (QUB-112) are the most worrying: the store-side API exists, the tests exist, and no caller writes a row or renders a view. Two other structural issues are real bugs today: the runs page's `failure_class` filter dropdown is wired to a field the store never reads, and the dashboard renders an N+1 telemetry fan-out that `webhook/dashboard.go:188` already solved differently for the JSON API.

---

## Findings

### SP-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking |
| Decide   | Change now |
| Location | `apps/receiver/internal/dashboard/dashboard.go:182-185` and `apps/receiver/internal/dashboard/templates/runs.html:14-22` — `runsFilter.FailureClass` + filter `<select>` |

**Observation:** `dashboard.go:184` adds `FailureClass` to the local `runsFilter` view-model and `runs.html:14-22` exposes a `<select name="failure_class">` with OOMKilled / Container error / Crash loop / Image pull / llm_timeout / rate_limit_429 options. The handler passes the URL value through to `runsFilter.FailureClass` (line 169) but never forwards it to the store: `serveRuns` calls `h.store.ListRuns(r.Context(), f)` where `f` is the bare `store.ListRunsFilter{Owner, Repo, Status}` (lines 134-141), and `store.ListRunsFilter` (`store/runs.go:255-264`) has no `FailureClass` field. The exceptions view does the class filter in Go (`serveExceptions:357-365`), but the runs page's filter chip is wired to a dead control.

**Impact:** An operator who picks "OOMKilled" on `/dashboard/runs` and clicks apply sees the unfiltered list, with no error and no indication anything is wrong. The exceptions view's chips look like the same control; they behave correctly because they go through `serveExceptions`. This is a UI/data contract mismatch that ships to the operator. The dropdown's `<option value="llm_timeout">` and `rate_limit_429` are also values that the K8s reconciler never writes — the only failure_class values actually produced are in `failureClassFromContainerState` (`webhook/k8s_reconcile.go:47`), which yields `oom_killed`, `container_error`, `crash_loop`, `image_pull`, `config_error`, and `container_<reason>` / `stuck_<reason>`. The dropdown is offering choices that can never match a row.

**Suggestion:**
- Add `FailureClass string` to `store.ListRunsFilter` and a `failure_class = ?` clause in `ListRuns`.
- Drop the two values from the dropdown that the reconciler never produces (or add a follow-up PR that maps runner-reported classes — `llm_timeout`, `rate_limit_429` — to the column too, then add a `runner` mapper in `k8s_reconcile.go`'s sibling file).
- Forward `q.Get("failure_class")` from `serveRuns` into `f.FailureClass` so the chip is the chip the operator sees.

---

### SP-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking |
| Decide   | Change now |
| Location | `apps/receiver/internal/store/audit.go:50,82` and `apps/receiver/cmd/receiver/main.go` — `RecordAuditEvent` / `ListAuditEvents` |

**Observation:** `RecordAuditEvent` and `ListAuditEvents` are defined, fully tested (`store_test.go:830-867`), and the audit_events table is created in migrations. The dashboard's nav (`templates/layout.html:53-58`) links Runs / Live / Exceptions / Costs / Installations — no audit link. The mutation paths that the audit log is meant to record — `Rerun` (`webhook/rerun.go:150`), `serveInstallationControl` (`webhook/dashboard.go:443`), and the K8s reconciler (`webhook/k8s_reconcile.go:161`) — all skip `RecordAuditEvent`. The "Actor" column is documented as "the BOOP_DASHBOARD_TOKEN bearer" but the dashboard middleware (`dashboard/dashboard.go:76-92`) does not stamp an actor into the request context. `RecordAuditEvent` rejects empty `Actor` (audit.go:51-53), so even if a handler tried to write, the writer has no source of identity.

**Impact:** QUB-112 ships a schema and a Go API surface, but the operator-visible feature is empty: no view, no mutation emits an event, no identity to attribute. The next operator incident ("who paused this install last week?") has no answer. The two follow-up PRs that wire this up — actor extraction from the middleware and audit writes in 3 handlers + a new `/dashboard/audit` route + a nav link — are now a single multi-file PR instead of landing naturally with the audit table commit.

**Suggestion:**
- Add the actor extraction now: have `dashboard.Handler.Middleware` set `ctx = context.WithValue(r.Context(), actorKey, h.token)` (or the first 8 chars for a real-identity future), and pass that context to `serveInstallationControl` and `Rerun`.
- Wrap the three mutating handlers with `h.store.RecordAuditEvent(ctx, store.AuditEvent{Action: "rerun.create", Actor: actorFromCtx(ctx), TargetID: id, Details: ...})`. Today's empty state is recoverable because no real data has been written; the next commit can land as audit-wiring, not audit-table.
- Add `serveAudit` and a nav link in a follow-up; the store-side `ListAuditEvents` is ready.

---

### SP-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer (one cleanup PR) |
| Location | `apps/receiver/internal/webhook/handler.go:1143-1150`, `apps/receiver/internal/webhook/rerun.go:242-257`, `apps/receiver/internal/store/rerun.go:78-89` — `buildJobName` / `buildJobNameRerun` / `buildJobNamePrefix` |

**Observation:** Three copies of essentially the same function. `handler.go:1145` is `buildJobName(owner, repo, number, sha)`; `rerun.go:255` is `buildJobNameRerun(owner, repo, pr, sha)`; `store/rerun.go:78` is `buildJobNamePrefix(owner, repo, pr, sha7)`. All three lowercase, sanitize with the same regex, prepend `boop-`, and join with `-`. `rerunJobNameSanitizer` (`rerun.go:253`) and `jobNameSanitizerRerun` (`store/rerun.go:89`) are literally the same regex. The rerun comment on `rerun.go:242-245` admits this explicitly: "shortSHARerun / buildJobNameRerun mirror handler.go's helpers. Duplicated here because handler.go's are unexported".

**Impact:** Adding a new Job-name component (e.g. QUB-110's reviewers asked for a `-r{n}` suffix — already done — or a future "include the run-id suffix" change) requires editing three files. The store copy is the load-bearing one for the count query, but the string is computed in the handler anyway; the store's only use is the SQL `LIKE` prefix, and that can take the `owner-repo-pr-sha7` raw string instead of a re-sanitized copy. The risk is silent divergence: a future change to the sanitization rules in handler.go (e.g. allow uppercase repo names) won't propagate to the rerun flow's `CountRerunJobsForSHA`, and the count goes off-by-one.

**Suggestion:**
- Move `buildJobName` and `shortSHA` into a tiny shared package, e.g. `internal/jobname`, with two exported functions. Have `handler.go`, `jobbuilder.go`, `rerun.go` (handler) and `store/rerun.go` (store) all import it.
- The store's `CountRerunJobsForSHA` keeps the regex check on the SQL row because the LIKE is approximate, but the prefix string it builds comes from the shared helper.

---

### SP-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now (small) |
| Location | `apps/receiver/internal/webhook/dashboard.go:428-438` — `parseRunStage` |

**Observation:** `parseRunStage` is a hardcoded switch mapping the runner's stage vocabulary to the receiver's `RunStatus`. The mapping is partial: it covers `running` / `succeeded` / `done` / `failed` and rejects anything else with 400. The runner's actual stage vocabulary lives in `workflow.mjs:82-233` (`STAGES`) where the status label for each macro stage is `auth`, `clone`, `review`, or `null` (silent). The dashboard's `live` view (`dashboard.go:308`) calls `ListRuns(..., Status: store.StatusRunning, ...)` to populate the "running" panel — a row that is `auth` or `clone` is not `running` in the store, so it doesn't show up. The receiver's `RecordStatus` accepts a stage string and stores it as-is on the run row (via `UpdateRunStatus`), so the row knows it was on `auth` — but the live view filters to `StatusRunning` only.

**Impact:** A run that is mid-`auth` or mid-`clone` does not show up on `/dashboard/live`. The waterfall (`renderWaterfall` at `dashboard.go:254`) shows the per-stage bars correctly because it reads `run_stages` rows, but the "live" KPI panel is empty. The runner's actual stage vocabulary (auth, clone, review) is in a separate file the receiver doesn't import. Adding a new macro stage means editing `workflow.mjs` (add the stage), the dashboard's `parseRunStage` (accept the new label), and the dashboard's `serveLive` (decide which statuses count as "live").

**Suggestion:**
- Expose the runner's stage vocabulary as a constant in `dashboard.mjs` (or a shared `lib/stages.mjs`): `export const RUNNER_STAGES = { auth: "running", clone: "running", review: "running", done: "succeeded", failed: "failed" }`. The receiver mirrors this in a Go constant.
- In `serveLive`, query the store with `Status IN (running, auth, clone, review)` (or a dedicated `ListLiveRuns` that returns "any non-terminal status"). Avoids the implicit two-source-of-truth.
- Alternative: the runner could POST to `/status` with a single canonical `status` value (running/succeeded/failed) and the per-stage waterfall comes from `/stages`. That eliminates the stage-vocabulary mirror entirely. The receiver comment in dashboard.go:374-378 explicitly considers the receiver's clock authoritative — extending that rule to status is the same idea.

---

### SP-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/receiver/internal/webhook/handler.go:135-173` — single `*Handler` struct with webhook + dashboard API + K8s reconciler + retention + backup + installation poller |

**Observation:** `*Handler` is now the receiver's app container. The struct holds `cfg`, `logger`, `kube`, `ghClient`, `store`, `dedup`, `limiter`, and the `consecutiveConfigMapFallbacks` counter. Its methods include webhook handling, deep health, dashboard data-layer GETs (8), runner POSTs (5), re-run API (2), installation control (1), installation poller start, retention loop start, backup loop start, and K8s reconciler start. Each of these reaches into a different subset of the struct fields. `cfg` alone is touched by the webhook path, the K8s reconciler (`TargetNamespace`), the dashboard API (`RunnerToken` for 4 endpoints), the retention loop (3 fields), the backup loop (3 fields), and the installations poller (1 field).

**Impact:** The next time someone needs to add a new background loop or a new POST endpoint, the diff is "add a field to `*Handler`, add a method, possibly add a config field to the `Config` struct in the same file". The struct does not enforce that "the dashboard API has no business reading `cfg.WebhookSecret`" or "the K8s reconciler does not need `RunnerToken`". There is no test seam — the integration tests construct one real `*Handler` and exercise it. The `ghClientAPI` interface at handler.go:118 is the one good seam in the file; the store has no equivalent.

**Suggestion:**
- For this PR: leave the struct alone (refactor would be a separate ticket; the cross-cutting is real but the next loop / endpoint is not on the immediate roadmap).
- For the store layer: define `type ReadStore interface { ListRuns(...); GetRun(...); GetTelemetry(...); ... }` and a `type WriteStore interface { UpsertRun(...); RecordTelemetry(...); ... }`. The dashboard depends on `ReadStore`; the runner POSTs and the webhook depend on `WriteStore`. The reconciler / K8s reconcile code depends on a third narrow interface (`SetRunFailureClass`). Tests then construct fakes per concern.

---

### SP-006
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/receiver/internal/dashboard/dashboard.go:147-174` (`serveRuns`) and `195-224` (`serveRunDetail`) — N+1 telemetry fan-out |

**Observation:** `serveRuns` calls `h.store.ListRuns` and then loops over each returned run calling `h.store.GetTelemetry` (line 161). For the default 50-row page that's 51 SELECTs. `webhook/dashboard.go:167-177` (the JSON API version of the same view) has the same loop. `RunWithTelemetry` (`webhook/dashboard.go:188-191`) exists to avoid this — it pairs the run with its telemetry in the JSON response — but the dashboard HTML view does not use it; it has its own `runsRow` (`dashboard.go:186-192`) and its own per-row `GetTelemetry` call. `serveRunDetail` does 4 separate calls (GetRun, ListRunStages, ListLensTelemetry, WalkLineage) on the same id; the first three are independent but the lens/stage queries could be one store method.

**Impact:** 50-row render = 51 queries. The store has an index on `telemetry(run_id)` and the call is cheap, but the dashboard refresh is "every few seconds during an incident" (dashboard.go:11). 51 queries × the dashboard's poll cadence × the operator's open tabs adds up. The pure HTML render path has not caught up to the JSON API's pattern.

**Suggestion:**
- Add a `ListRunsWithTelemetry` to the store (or have `ListRuns` accept a `WithTelemetry bool` flag) that joins telemetry in a single query. `serveRuns` uses it; the JSON API in `webhook/dashboard.go:167` also simplifies to one call.
- For the per-detail view, add a `GetRunFull` that returns `{Run, Stages, Lenses, Lineage}` in one transaction (or one round-trip). The waterfall math (`renderWaterfall`) stays in the dashboard package.

---

### SP-007
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/workflow.mjs:279` — `const postStage = deps.postStage || defaultPostStage;` |

**Observation:** The override path is dead. The runner's `deps` bundle (constructed in `index.mjs:80-135`) does not have a `postStage` field; it has `postStatus` (the GitHub comment PATCH) and `postDashboardStatus` (the dashboard `/status` POST). The `deps.postStage || defaultPostStage` line always falls through to `defaultPostStage` from `dashboard.mjs`. The comment at workflow.mjs:68-71 says "a test that wants to drive stages without a dashboard can pass `overrides.postStage = noop`", but `overrides` is the third argument to `runStages` and is never merged into `deps` before the `deps.postStage` read. A test that wants no-op stage POSTs has to construct a `deps` with a `postStage` field itself; the `overrides` channel doesn't reach this line.

**Impact:** Minor. The default path works (every test gets the real postStage, which is harmless because `postWithRetry` swallows errors). But the override pattern is misadvertised. The `overrides.runOpenCodeSkill` / `overrides.gather` etc. hooks at workflow.mjs:705, 734, 751, 793 work because they are read directly off `overrides`, not off `deps`. A future test that follows the documented `overrides.postStage = noop` recipe will silently not override anything.

**Suggestion:**
- Either move the `postStage` resolution to read `overrides.postStage` (matching the other override reads) or fix the documentation. The smallest change: change the read to `overrides.postStage || defaultPostStage` to match the other override consumers in the file.

---

### SP-008
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now (small) |
| Location | `apps/receiver/internal/store/audit.go:145-181` and `apps/receiver/internal/dashboard/templates/` — `ListRetentionSchedule` and missing UI |

**Observation:** `ListRetentionSchedule` is defined (store/audit.go:145) and tested (store_test.go:875) but has no dashboard route. The operator's "how long until this run is pruned?" question — which the docstring at audit.go:120-125 explicitly calls out — has no answer surface. The `retention` table mentioned in the docstring does not exist; the function returns rows computed on the fly from `runs.started_at + retention`. The dashboard's run-detail template (`templates/run_detail.html`) does not render the scheduled-deletion date. There is no nav link, no template, no handler. The store-side function is correct but is reading state that the operator cannot act on.

**Impact:** Like SP-002, this is "the API exists; the feature doesn't." The retention job is running (the receiver starts `StartRetentionLoop` at main.go:150), so rows are actually being deleted; the operator just cannot predict which rows are about to disappear. A `cmd/receiver/main.go` log line at startup emits the effective retention; that's the only operator-visible signal today.

**Suggestion:**
- Add a `/dashboard/retention` route and a `retention.html` template that renders a "Days until scheduled delete" column for every run, with the "imminent" flag (under 7 days) as a row class. Use the existing `ListRetentionSchedule` method.
- For the run-detail page, add a single KPI on `templates/run_detail.html` showing "scheduled delete" with the timestamp.

---

### SP-009
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/index.mjs:46-52, 280-360` — runner lifecycle wires GitHub status + dashboard status in two parallel paths |

**Observation:** The success and catch paths in `index.mjs` post to both the GitHub status comment and the dashboard. The two functions have identical signatures, but the import line shadows the names: `postStatus` (line 41) is from `github.mjs`; `postStatus as postDashboardStatus` (line 47) is from `dashboard.mjs`. The two are kept in lockstep manually: line 333-334 and 343-348 each call both. The status vocabulary is the runner's `STAGES` IDs (`done`, `failed`, `auth`, `clone`, `review`) — the dashboard's `parseRunStage` only accepts four of those (see SP-004). The runner's STAGES list has `null` for silent sub-stages (gather / meta-review / narrate) but those never get a `/status` POST, so the asymmetry is silent.

**Impact:** The shadowed name is fragile. A future contributor who searches for "postStatus" in `index.mjs` will see the dashboard version (because it's imported last in the block) and the GitHub version in `github.mjs`. The two functions have the same signature, but they POST to different URLs and take different context fields. The `Q`-filter on the imports means a re-ordering (alphabetical, for instance) would silently swap which one is bound to the bare name. The lockstep dual-posting is also duplication — if a third channel ever lands (Slack, PagerDuty), each call site grows by one line.

**Suggestion:**
- Rename the dashboard import to `postDashboardStatus` at the use site too (not just the import alias), so the symbol name in the body matches the source. The current code already does this; the issue is the `postStatus` import name (line 41) is the one most people will pattern-match on. Renaming the GitHub one to `patchGitHubStatus` (or the runner's internal wrapper) eliminates the shadow.
- For the dual-posting: extract a `notifyLifecycle(stage, reason, ctx, deps)` helper that fans out to GitHub + dashboard + (future) other channels. The success and catch blocks each call one function.

---

### SP-010
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/receiver/internal/webhook/dashboard.go:667-780` and `124-182` — `StartInstallationsPoller`, `StartRetentionLoop`, `StartBackupLoop` live in the webhook package |

**Observation:** Three background loops (installation poller, retention, backup) are started by methods on the `*Handler` struct in the `webhook` package, even though none of them touch the webhook path. The K8s reconciler in `k8s_reconcile.go` is a fourth. The `webhook` package's job is "verify the X-Hub-Signature-256, dispatch the event." It now also owns four long-running goroutines. The pattern is the same in all four: `if h.store == nil { return noop }`; `interval` floors; a 15-second start delay; a `ticker.Reset(interval)` loop with a per-tick context.WithTimeout.

**Impact:** None today. The next background loop (metrics emission? rate-limit tracking?) will land in the same file and the boilerplate will be the fifth copy. The `webhook` package's surface area is now ~6x what its name suggests, and `cmd/receiver/main.go:147-163` calls 4 `Start*` methods on `*Handler` in sequence with `defer stopFunc()` lines that are easy to forget when adding the 5th.

**Suggestion:**
- Extract a `BackgroundLoops` type in the `webhook` package (or a new `internal/loops` package) that owns the start/stop and the boilerplate. `*Handler` exposes a `StartBackgroundLoops(ctx, cfg)` that fans out to each loop. Today this is optional; when the next loop lands, do it then.

---

## Structural Snapshot

```
                         ┌────────────────────────────────────────────┐
                         │     apps/receiver (Go binary)              │
                         └────────────────────────────────────────────┘

        ┌──────────────────┐    ┌────────────────────┐    ┌─────────────────────┐
        │ cmd/receiver/    │───▶│  internal/webhook/ │───▶│  internal/store/    │
        │ main.go          │    │  handler.go (47K)  │    │  store.go (Open)    │
        │                  │    │  dashboard.go (NEW)│    │  runs.go            │
        │  - 4 Start*()    │    │  rerun.go (NEW)    │    │  stages.go          │
        │  - 1 mux + 7     │    │  k8s_reconcile.go  │    │  audit.go (NEW)     │
        │    POST routes   │    │  reviews.go        │    │  rerun.go (NEW)     │
        │                  │    │  jobbuilder.go     │    │  installations.go   │
        │                  │    │  handler.go (NEW   │    │  lens_telemetry.go  │
        │                  │    │   +37 lines:       │    │  retention.go       │
        │                  │    │   isPaused)        │    │  backup.go          │
        └──────────────────┘    │                    │    │  migrations.go      │
                                │  One *Handler with │    │  (v1-v5)            │
                                │  12+ methods and   │    │  stats.go           │
                                │  4 background      │    └─────────────────────┘
                                │  loops.            │              │
                                └────────┬───────────┘              │
                                         │  *store.Store (concrete, no interface)
                                         ▼                              ┌─────────────┐
                                ┌────────────────────┐                   │  K8s API    │
                                │  internal/dashboard│                   │  (jobs,     │
                                │  dashboard.go      │◀── embed.FS ─────│  pods)      │
                                │  templates/*.html  │                   └─────────────┘
                                │  (server-rendered  │
                                │   html/template)   │                   ┌─────────────┐
                                └────────┬───────────┘                   │  GitHub API │
                                         │                               │  (Octokit)  │
                                         ▼                               └─────────────┘
                              [ operator browser ]

        ┌────────────────────────────────────────────┐
        │     apps/runner (Node.js, in-pod)          │
        └────────────────────────────────────────────┘

        ┌──────────────────┐    ┌────────────────────┐    ┌─────────────────────┐
        │ src/index.mjs    │───▶│ src/lib/workflow   │───▶│ src/lib/dashboard   │
        │ (orchestrator)   │    │  .mjs (STAGES,     │    │  .mjs (NEW: best-   │
        │  - 2xx            │    │   runStages,       │    │   effort POSTs to   │
        │  - try/catch/    │    │   withRetry)       │    │   /api/runs/.../stages│
        │    finally       │    │                    │    │   .../heartbeat,    │
        │  - dual post to  │    │  postStage         │    │   .../telemetry,    │
        │    GitHub +      │    │  injected via      │    │   .../lens_telemetry│
        │    dashboard     │    │  deps.postStage || │    │   .../status)       │
        └──────────────────┘    │  defaultPostStage  │    └──────────┬──────────┘
                                └────────────────────┘               │
                                         │  deps bundle             │
                                         │  (fetch, log, etc.)      │
                                         ▼                          │  HTTP POSTs
                                                                  ───┘
                                                                  (in-cluster,
                                                                   best-effort)

   ╔══════════════════════════════════════════════════════════════════════╗
   ║ Cross-binary dependency: runner → receiver                          ║
   ║   URL hardcoded in dashboard.mjs:                                   ║
   ║     ${ctx.dashboardUrl}/api/runs/{jobName}/stages                   ║
   ║   Header hardcoded:                                                 ║
   ║     X-BOOP-Runner-Token: ${ctx.dashboardToken}                      ║
   ║   Stage vocabulary shared by convention only                        ║
   ║   (see SP-004). No shared types / schema between the two binaries.  ║
   ╚══════════════════════════════════════════════════════════════════════╝
```

**Summary of seams:**
- **Receiver internal:** `ghClientAPI` (handler.go:118) is the only interface; the store is a concrete `*store.Store` everywhere. The dashboard handler has `*store.Store` directly; the runner-POST handlers have it directly. There is no `ReadStore` / `WriteStore` interface split.
- **Receiver → K8s:** `h.kube kubernetes.Interface` (handler.go:138) — well-typed, swappable in tests.
- **Runner internal:** clean `deps`-bundle pattern. `index.mjs:80-135` constructs the bundle; each lib module reads only what it needs. Tests inject fetch / spawn / log / etc. The `postStage` override is the one inconsistency (SP-007).
- **Runner → receiver:** URL + header by string. No shared schema. Stage vocabulary is duplicated by convention (SP-004).
- **Operator → receiver:** `BOOP_DASHBOARD_TOKEN` gates `/dashboard/*`; an empty value 401s everything (dashboard.go:78-80). The GET API endpoints (`/api/runs`, `/api/stats`, etc.) are NOT token-gated — the comment at webhook/dashboard.go:10-13 says the auth layer "sits in front of the dashboard, not in the receiver" if it ever moves behind an Ingress. Today there is no such fronting, so a misconfigured cluster exposes the data layer to anything that can hit the service.

**What is easy to test today:** `*Handler` with fakes for `ghClientAPI` and `kube`. The store via `store_test.go`. The runner lib via `dashboard.test.mjs` (overrides `fetchImpl`).

**What is hard to test today:** The dashboard's HTML render path (templates are real, no test invokes `serveRuns` against a fake store). The N+1 telemetry fan-out is not asserted anywhere. The K8s reconciler's per-tick behavior (`k8s_reconcile.go:161`) is not directly tested; only the store round-trip is. The retention and backup loops are not unit-tested.
