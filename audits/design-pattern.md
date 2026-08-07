# Structural Review
**Date:** 2026-08-06
**Scope:** `feature/qub-112-dashboard-cross-cutting` — QUB-108 + QUB-109 + QUB-110 + QUB-111 + QUB-112 (5 stacked commits)

Files in scope (per the diff, +3946/-46):
- `apps/receiver/cmd/receiver/main.go`
- `apps/receiver/internal/dashboard/dashboard.go` + `templates/*.html` (7 files)
- `apps/receiver/internal/store/audit.go`, `installations.go`, `lens_telemetry.go`, `migrations.go`, `rerun.go`, `runs.go`, `stages.go`
- `apps/receiver/internal/webhook/dashboard.go`, `handler.go`, `k8s_reconcile.go`, `rerun.go`
- `apps/runner/src/index.mjs`, `lib/dashboard.mjs`, `lib/dashboard.test.mjs`, `lib/workflow.mjs`

---

## Summary

The cross-cutting work adds five new tables, a server-rendered dashboard, a runner-side POST surface, and a K8s reconciler. Most of the new code is in the right place: the store owns SQL, the dashboard owns view math, the webhook owns the wire surface. Three things are actively wrong: the lineage view is wired to a field name the struct does not have (always shows "no parent"), the job-name format is duplicated across three files (the in-code comment admits it), and the audit log / zero-out / refund table producers do not exist anywhere in the tree even though the migration is in. The dashboard HTML embeds `store.Run` directly at the wire boundary, and the dashboard module is one 497-line file with six concerns; both will hurt when the next view lands.

---

## Findings

### DP-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/dashboard/templates/run_detail.html:55-66` reads `.Lineage.Up` / `.Lineage.Down`; `apps/receiver/internal/store/rerun.go:99-102` defines the field as `WalkUp` / `WalkDown` |

**Observation:** The dashboard's run-detail template walks `.Lineage.Up` and `.Lineage.Down`. The store returns `store.Lineage{WalkUp, WalkDown}`. There is no code path that renames the fields between the two — `dashboard.go:209` assigns `data.Lineage = lineage` and `data` is the template root, so the template reads `Lineage.WalkUp` and `Lineage.WalkDown`. The `{{if .Lineage.Up}}` branch is permanently false; the "No parent (this is the root of a chain)" copy at line 61 always renders, even for a run with a real `parent_run_id`.

**Impact:** The Phase 3 re-run lineage view — the load-bearing reason to add the `parent_run_id` / `superseded_by_id` columns in v4 — is silently broken. An operator who clicks a re-run row will see the "is the root" empty state, not the chain. The store test `TestUpsertRun_LineageRoundTrip` exercises the Go-side fields and so passes, masking the wire-side bug.

**Suggestion:** Pick one side and match the other. Either rename the Go fields to `Up` / `Down` (matches the template and the prose), or change the template to `.Lineage.WalkUp` / `.Lineage.WalkDown`. Renaming the template is the lower-blast-radius change — Go callers outside this file do not exist yet, and the prose / spec already use the short form.

```go
// rerun.go:99
type Lineage struct {
    Up   []Run
    Down []Run
}
```

```html
<!-- run_detail.html:55 -->
{{if .Lineage.Up}}
```

---

### DP-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/webhook/handler.go:1143-1154` (`buildJobName` / `shortSHA` / `jobNameSanitizer`), `apps/receiver/internal/webhook/rerun.go:235-258` (`buildRerunJobName` / `buildJobNameRerun` / `shortSHARerun` / `rerunJobNameSanitizer`), `apps/receiver/internal/store/rerun.go:78-89` (`buildJobNamePrefix` / `jobNameSanitizerRerun`) |

**Observation:** The job-name format `boop-{owner}-{repo}-{pr}-{sha7}` and its re-run suffix `-r{n}` are encoded three times across the codebase. Each copy owns its own sanitizer regex and its own `shortSHA` impl. The store-side `buildJobNamePrefix` carries a comment that literally reads "The handler's version is unexported and the rerun code lives in a different package, so the regex is duplicated here."

**Impact:** A format change today (e.g. adding a `-v{epoch}` suffix, or a longer SHA, or supporting a forward-slash in repo name) requires editing three files, plus a test update in each, plus the handler's `handler_test.go:158-171` tests. The dashboard's `CountRerunJobsForSHA` query relies on the suffix being literally `-r{N}` at end-of-string, so a silent drift in one copy will leave the dashboard counting re-runs from the wrong table slice. The current `SELECT id FROM runs WHERE id LIKE ? || '%' AND id LIKE '%-r%'` plus the regex re-check is a workaround for exactly this duplication — without the regex re-check, a row with an `r` in the middle of its id (e.g. a future "branch" name) would be miscounted.

**Suggestion:** Move the format to a single package (the `store` package is the natural home — it already stores the id and is the source of truth for what counts as a re-run). Export `BuildJobName`, `BuildRerunJobName`, and `ShortSHA` from there. Replace the three copies. The handler's `jobNameSanitizer` becomes an alias or a re-export.

```go
// store/jobs.go (new)
func BuildJobName(owner, repo string, pr int, sha string) string { ... }
func BuildRerunJobName(owner, repo string, pr int, sha7 string, n int) string { ... }
func ShortSHA(sha string) string { ... }
```

```go
// handler.go:1145
func buildJobName(owner, repo string, number int, sha string) string {
    return store.BuildJobName(owner, repo, number, sha)
}
```

---

### DP-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/store/audit.go:50`, `apps/receiver/internal/store/stages.go:211`, `apps/receiver/internal/dashboard/templates/exceptions.html:35-37` |

**Observation:** Three of the new store methods have zero producers in this PR:

- `RecordAuditEvent` — the migration `migrateV5` creates `audit_events` with `actor NOT NULL` and the migration comment calls "actor" the load-bearing column, but no webhook or dashboard handler calls `RecordAuditEvent`. The migration comment lists the planned producers (`rerun.create`, `install.pause`, `install.resume`, `lens_opt_out.set`, `cost.zero_out`, `webhook.hmac.fail`, `webhook.hmac.pass`); none of them exist in code.
- `RecordRefund` — the `refunds` table is in v2 and the store method exists, but the exceptions dashboard references a `<form method="post" action="/dashboard/runs/{{.ID}}/zero-cost">` button at `exceptions.html:35`. No such route is registered. Clicking the button is a 404.
- `SetInstallationControls` is wired (the pause toggle in the installations view works), so the schema half is real — but the audit column for that action does not get written.

**Impact:** A future PR that adds the missing handlers has to remember to also call `RecordAuditEvent`, `RecordRefund`, etc. The migration ships the table and the column today, so production rows will accumulate audit events only when somebody remembers to call the producer. The "compliance audit" promise in the migration comment is empty until the producers land.

**Suggestion:** Either land the producers in this PR, or split the migration so the audit_events table is a follow-up. If the producers are deliberately out of scope, the migration should be the table only — drop the "actor is the load-bearing piece" framing from the comment so a reader does not look for the producer and conclude it was lost.

---

### DP-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/dashboard.go:188-191` (`RunWithTelemetry`), `apps/receiver/internal/dashboard/dashboard.go:186-192, 233-239, 347-353, 432-436` (`runsRow` / `runView` / `liveRow` / `exceptionRow` / `installRow` all embed `store.Run` / `store.Installation`) |

**Observation:** Every dashboard view type and the JSON API's `RunWithTelemetry` embed the `store.Run` struct directly, so a new column on `store.Run` automatically appears on the wire. The dashboard templates reach into embedded fields: `{{.ID}}`, `{{.Owner}}`, `{{.Repo}}`, `{{.FailureClass}}`, `{{.PRNumber}}`, `{{.CommitSHA}}` — all sourced from the embedded struct. The `installation_id` column is in the `runs` table and is therefore also embedded; the dashboard never displays it, but the JSON `/api/runs` response returns it to the operator UI.

**Impact:** Today, the only fields on `Run` are review-lifecycle data the operator is allowed to see. The next column added (an internal cost-rate, a PII-bearing comment author's email, an OAuth token hint) silently leaks to the dashboard and the JSON API. There is no review surface that catches this; the `embed` is the implicit allowlist. `RunWithTelemetry` is the worst offender because it ships over the same wire the runner's POST endpoints use — a future per-row debug payload added to `store.Run` would land in the JSON the dashboard reads.

**Suggestion:** Define a `view.RunSummary` (and `view.InstallationSummary`) struct that picks the fields the dashboard is allowed to see, and have the handler convert. The Go-struct conversion is mechanical and the template can stay. For the JSON API, the same struct doubles as the response body.

```go
// internal/view/run.go (new)
type RunSummary struct {
    ID            string
    Owner, Repo   string
    PRNumber      int
    CommitSHA     string
    Status        string
    StartedAt     string
    EndedAt       string
    Duration      string
    FailureClass  string
    ParentRunID   string
}

func FromStoreRun(r store.Run) RunSummary { ... }
```

Defer until a sensitive field actually lands — but the convert now, before the second one, or the second one will be the leak.

---

### DP-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/dashboard.go:484-528` (`RecordStage` accepts any string), `apps/receiver/internal/store/migrations.go:233-242` (`run_stages` schema), `apps/runner/src/lib/workflow.mjs:82-156` (`STAGES` enum) |

**Observation:** The `stage` column in `run_stages` is a free-form `TEXT NOT NULL`. The receiver's `RecordStage` accepts any string and stores it. The runner's `STAGES` array in `workflow.mjs` is the producer — today `handshake / fetch / sniff / summary / inlines / cleanup` plus the sub-stages `classify / dispatch / gather / meta-review / narrate`. Compare this to the `failure_class` column on `runs`, which has a normalized enum (`oom_killed / container_error / crash_loop / image_pull / ...`) produced by a single `failureClassFromContainerState` mapping in `k8s_reconcile.go:47-103` that owns the K8s surface.

**Impact:** A new stage name added to the runner (e.g. `lens_select` for QUB-95) lands in the table with no schema-level constraint. The dashboard's waterfall renders it as a bar but cannot filter on it (the "stages" dropdown doesn't exist; compare to the "failure class" dropdown in `runs.html:14-20`). The K8s-side normalization rule was applied to `failure_class` but not to `stage`. A future PR that wants a "stages list" filter will have to either (a) regex-parse the table, or (b) backfill a new `stage_kind` enum column.

**Suggestion:** Add a typed `StageKind` enum (or a small `stages` lookup table) and have `RecordStage` validate against it. The validation belongs at the wire boundary, not in the runner — the runner trusts the receiver to canonicalize, just like it trusts the K8s API to canonicalize exit reasons. Alternatively, mirror the `failureClassFromContainerState` shape: a single `normalizeStage(s string) (StageKind, bool)` function in the receiver that the runner does not need to know about.

---

### DP-006
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/dashboard/dashboard.go` (497 lines) |

**Observation:** `dashboard.go` is one file with six concerns: 6 view handlers (`serveRuns` / `serveRunDetail` / `serveLive` / `serveExceptions` / `serveCosts` / `serveInstallations` / `serveInstallationControl`), the route table, the waterfall math (`renderWaterfall`, `durMS`), the auth middleware, a `Health` endpoint, a dead `renderJSON` helper, and a `var _ = context.Background` import-keeper at line 497. The 6 view structs (`runsView`, `runDetailView`, `liveView`, `exceptionsView`, `costsView`, `installationsView`) and the 6 row structs (`runsRow`, `runView`, `stageRow`, `liveRow`, `exceptionRow`, `installRow`) are all declared in this file.

**Impact:** A new view adds ~50-100 lines to the same file, and the file is already past 500 lines. The waterfall math is presentation logic in a file named after the dashboard package; the row structs are view models in a file named after the package. The `renderJSON` helper is dead code (no caller; the comment says "not wired to a route but kept around"). The `var _ = context.Background` is an import-keeper hack — the file imports `context` and never uses it. Both are signs the file was assembled in a hurry.

**Suggestion:** Split the file along the obvious seams:

- `dashboard/router.go` — `Handler`, `NewHandler`, `Middleware`, `RegisterRoutes`, `route` table, `Health`
- `dashboard/views/runs.go`, `views/live.go`, `views/exceptions.go`, `views/costs.go`, `views/installations.go` — one per page
- `dashboard/waterfall.go` — `renderWaterfall`, `durMS`, the `stageRow` view struct

Delete `renderJSON` and the `var _ = context.Background` line in the same PR. The `context` import is unused; remove it instead of papering over it.

---

### DP-007
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/dashboard/dashboard.go:195-224` (`serveRunDetail`) |

**Observation:** `serveRunDetail` reads the run, the stages, the lens telemetry, and the lineage in four separate store calls. There is no transactional boundary — each call is a fresh snapshot. The waterfall can disagree with the run row (a stage POST landing mid-render, a re-run lineage pointer changing between the two reads).

**Impact:** Today the inconsistency window is small (a single dashboard page render) and the data is read-only, so the worst visible artifact is a waterfall that "jumps" when the operator refreshes. The cost is small. The cost grows when a future "run detail" feature wants the "render the most recent refund for this run" or "render the latest failure_class tick" — each will be another store call, and the rule "more reads = more opportunities to disagree" is the one that bites.

**Suggestion:** Add a `GetRunDetail(ctx, id) (RunDetail, error)` store method that returns a single struct (Run + Stages + Lenses + Lineage) from one transaction. The store already does this for `SetInstallationControls` (one transaction, two writes); the read-side equivalent belongs next to it.

---

### DP-008
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                        |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/webhook/dashboard.go:51-108` (`ListInstallations` cold-start refresh) vs `apps/receiver/internal/dashboard/dashboard.go:413-415` (`serveInstallations` — no refresh) |

**Observation:** The JSON `GET /api/installations` handler does a synchronous GitHub refresh on a cold start (table empty + no recent fetch). The HTML `/dashboard/installations` page does not. So a fresh install that hits the dashboard first sees an empty list until the 5-minute poll catches up; a fresh install that hits the JSON API first sees a populated list. The two surfaces disagree on the same data.

**Impact:** Low. Both surfaces eventually converge on the same row set. The cost of the inconsistency is a few minutes of "empty installations" on day one of a new deploy.

**Suggestion:** Either give `serveInstallations` the same cold-start fallback (lift the refresh block into a small `installationsCache` method on the store that both call), or accept the asymmetry and document it on the dashboard page. The smaller fix is to lift the refresh into a `store.RefreshInstallationsIfStale(ctx, maxAge)` helper that both handlers call.

---

### DP-009
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                        |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/webhook/dashboard.go:167-172` and `apps/receiver/internal/dashboard/dashboard.go:154-164` |

**Observation:** Both the JSON `GET /api/runs` handler and the HTML `serveRuns` page call `ListRuns`, then iterate the result and call `GetTelemetry` per run. With the default `Limit: 50`, that's 1 + 50 = 51 SELECTs. Same N+1 shape in both places.

**Impact:** Today the runs page is operator-only and the row count is small. The shape is the same one that grows quadratic once a dashboard view wants per-row refund history or per-row lens totals (each new column becomes another per-row call).

**Suggestion:** Add a `ListRunsWithTelemetry(ctx, filter) ([]RunWithTelemetry, error)` store method that JOINs the runs and telemetry tables in one query (a LEFT JOIN with the aggregate telemetry). The `RunWithTelemetry` type already exists; it just needs a single-query producer. Same shape can be extended to LEFT JOIN lens_telemetry aggregates when the dashboard needs "cost by lens" on the runs list.

---

### Unable to verify

- **Stage → status enum drift between runner and receiver** — the receiver's `parseRunStage` (`webhook/dashboard.go:428-438`) maps `running/succeeded/done/failed` to `RunStatus`, and the runner's `STAGES[].statusStage` (`workflow.mjs:82-156`) maps stage ids to labels. These are two independent maps with different domains. In practice, the runner calls `postDashboardStatus` only with `done` or `failed` (via `index.mjs:311, 333, 343`), so the wide `parseRunStage` is not exercised today. I did not trace every historical call site; a past PR may have changed the call site. To confirm: search for `postDashboardStatus` callers and check the `stage` string they pass.
- **`scanRun` scan-shape fragility** — `runs.go:407-468` is a 60-line hand-written scan that touches every column. With v1→v5 the column list has grown from 13 to ~20, and the v4/v5 columns (`failure_class`, `last_heartbeat_at`, `parent_run_id`, `superseded_by_id`) all sit in the middle of the SELECT. A struct-tag-driven scan would be more maintainable. I did not call this out as a finding because no production change is blocked today; a v6 schema change will hurt but that is a future-PR problem.

---

## Tier summary

- 🔴 Blocking (3): DP-001 (lineage field name mismatch), DP-002 (job-name duplication across 3 files), DP-003 (audit + refund + zero-cost producers missing)
- 🟡 Follow-up (4): DP-004 (Run embedded at the wire boundary), DP-005 (stage name unconstrained), DP-006 (dashboard.go is one 497-line file with dead code), DP-007 (serveRunDetail's 4-call read is not transactionally coherent)
- 🟢 Optional (2): DP-008 (installations cold-start asymmetry), DP-009 (N+1 telemetry query in runs list)
