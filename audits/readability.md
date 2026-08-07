# Readability Audit
**Date:** 2026-08-06
**Scope:** QUB-108 / QUB-109 / QUB-110 / QUB-111 / QUB-112 — `feature/qub-112-dashboard-cross-cutting`

Files reviewed (changed code only):
- `apps/receiver/cmd/receiver/main.go` (+36)
- `apps/receiver/internal/dashboard/dashboard.go` (+497, NEW FILE)
- `apps/receiver/internal/dashboard/templates/*.html` (7 files, NEW)
- `apps/receiver/internal/store/audit.go` (+200, NEW)
- `apps/receiver/internal/store/installations.go` (+/-/0)
- `apps/receiver/internal/store/lens_telemetry.go` (+204, NEW)
- `apps/receiver/internal/store/migrations.go` (+/-/0)
- `apps/receiver/internal/store/rerun.go` (+161, NEW)
- `apps/receiver/internal/store/runs.go` (+185/-/0)
- `apps/receiver/internal/store/stages.go` (+301, NEW)
- `apps/receiver/internal/store/store_test.go` (+464, NEW)
- `apps/receiver/internal/webhook/dashboard.go` (+202, NEW)
- `apps/receiver/internal/webhook/handler.go` (+37)
- `apps/receiver/internal/webhook/k8s_reconcile.go` (+257, NEW)
- `apps/receiver/internal/webhook/rerun.go` (+258, NEW)
- `apps/runner/src/lib/dashboard.mjs` (+117, NEW)
- `apps/runner/src/index.mjs` (+/-/0)
- `apps/runner/src/lib/workflow.mjs` (+/-/0)

---

## Summary

Across this five-commit stack the code reads well at the per-function level — every new public function carries a doc comment that explains the *why* (QUB ticket reference, one-clock rule, retry contract, etc.), template strings are embedded, and the store/webhook boundary keeps a clean "store returns sentinel, handler maps to HTTP" shape. The standout positive is `k8s_reconcile.go`: `failureClassFromContainerState` / `mapExitReason` / `mapWaitingReason` are a tiny, named, single-purpose trio that reads as a vocabulary mapping rather than a flow. Two patterns will slow the next reader down if left alone: (1) the new `webhook/rerun.go` re-declares `shortSHARerun` / `buildJobNameRerun` / `rerunJobNameSanitizer` to mirror unexported `handler.go` helpers, so the Job-name convention is now load-bearing knowledge held in two files; (2) the "unknown run" detection at the HTTP boundary uses three different shapes in two files — `store.ErrUnknownRun` in `RecordTelemetry` and the re-run handlers, but raw `sql.ErrNoRows` in `RecordStatus` and `RecordHeartbeat` — so a reader has to check which one each endpoint asserts on. The `var _ sql.NullString` placeholder at the bottom of `store/audit.go` is a third one-line issue that should die before it gets copy-pasted.

---

## Findings

### RD-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/store/audit.go:195-200` — `var _ sql.NullString` placeholder |

**Observation:** The file ends with

```go
// sql is imported for the future use of sql.NullString
// in the audit event's details column scan path. The
// compiler complains about an unused import if we
// remove the import; the alias is here as a guard
// against a future refactor that needs it.
var _ sql.NullString
```

The import is *not* used elsewhere in the file: the scan at `audit.go:106` uses bare `&ev.Details string` against `COALESCE(details, '')` (the column is COALESCEd in SQL, so the scan does not need `sql.NullString`). The comment claims the placeholder is "for the future use" of `sql.NullString` in the *scan* path, but the scan is already written and does not use it. The whole `database/sql` import is then load-bearing only to support this one dead identifier.

**Impact:** Three minor things, each small on its own:
1. The `var _ sql.NullString` line is a code smell. A reader who skims the file bottom for "where is `sql` used?" finds this and has to read the comment to learn it isn't used at all. The next maintainer who adds a new method that legitimately needs `database/sql` will either remove this line (correct) or be confused into leaving it (wrong).
2. The comment is forward-looking ("future use", "guard against a future refactor") in a way that documents a *non-event* — no reader arriving at this code is making a decision that depends on knowing this placeholder is intentional.
3. The audit's `ListAuditEvents` reads `details` as a plain `string` and COALESCEs the column. The package's `nullString` helper (`runs.go` etc.) already handles the empty-string convention; there is no path through this file that would benefit from `sql.NullString`.

**Suggestion:** Drop the import and the placeholder. The whole tail becomes:

```go
// (delete the comment block and the var _ line, and the
// "database/sql" import on line 20)
```

The remaining file compiles and behaves identically. The "use sql.NullString in a future refactor" guidance can be inlined in the place that needs it (a one-line `// scan via sql.NullString here if details becomes nullable` comment next to a future scan, not a 5-line block at the bottom of *this* file).

---

### RD-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/rerun.go:242-257` — duplicated `shortSHARerun`, `buildJobNameRerun`, `rerunJobNameSanitizer`; mirror copies in `apps/receiver/internal/store/rerun.go:38-89` |

**Observation:** The Job-name convention `boop-{owner}-{repo}-{pr}-{sha7}` and the `-r{n}` suffix is held in three places:

1. `webhook/handler.go:1143-1151` — `jobNameSanitizer`, `buildJobName`, `shortSHA` (the originals, lowercase, package-internal)
2. `webhook/rerun.go:246-257` — `shortSHARerun`, `buildJobNameRerun`, `rerunJobNameSanitizer` (re-declared with the `Rerun` suffix so they don't collide with the unexported originals)
3. `store/rerun.go:38, 78-89` — `rerunSuffix`, `buildJobNamePrefix`, `jobNameSanitizerRerun` (a *third* copy, this one in a *different package*)

The doc comment on `rerun.go:242-245` admits the duplication: *"Duplicated here because handler.go's are unexported and the re-run flow needs them. The implementations match exactly."* The store-side comment at `store/rerun.go:85-88` admits the same thing: *"The handler's version is unexported and the rerun code lives in a different package, so the regex is duplicated here."*

The `-r{n}` suffix regex (`-r(\d+)$`) lives only in `store/rerun.go`. The sanitizer regex (`[^a-z0-9-]`) lives in all three. The 7-char SHA truncation logic lives in both webhook files (and not in the store, which receives an already-truncated `sha7` from the caller).

**Impact:** The convention is now a "tribal" piece of code: the next author who needs to add a `-r{n}` variant (e.g. for parallel review attempts) has to find all three copies and edit them in lockstep. A subtle drift is already latent: `store/rerun.go:47` filters `id LIKE ? || '%' AND id LIKE '%-r%'` which is *wider* than the `-r(\d+)$` regex the same file then re-validates with at `rerun.go:64` — the LIKE filter is what `CountRerunJobsForSHA` then re-checks, so the count is correct, but a reader has to follow both to confirm.

A future change to the prefix format (e.g. for a wider K8s name budget, or to add a bot-name disambiguator) has three edit sites, two of which are explicitly noted as "kept in sync with handler.go". The test file `handler_test.go:159-171` pins the originals; nothing pins the rerun copies.

**Suggestion:** Move the Job-name convention into one shared package. Two options:

1. Promote `buildJobName` / `shortSHA` / `jobNameSanitizer` to exported symbols in a small new `internal/jobname` package (or just in `internal/store` if the store is the right home). The webhook handler, the re-run handler, and the store's `CountRerunJobsForSHA` all import the same `JobName(owner, repo, pr, sha7)` and `ShortSHA(sha)` functions. The re-run adds a thin `RerunJobName(owner, repo, pr, sha7, n)` on top. Two regexes become one constant.
2. Make the store's `CountRerunJobsForSHA` self-contained: take the full `(owner, repo, pr, sha7)` tuple, compute the prefix internally, and stop relying on the caller to construct the same string. The `prefix` arg goes away.

Either is a non-trivial refactor (handler.go, rerun.go, store/rerun.go, and the test file all need to be in sync), so defer to the next time the Job name format needs to change. The cost of not doing it is "one more copy" every time the convention grows.

---

### RD-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/dashboard.go:345, 407, 557` — inconsistent "unknown run" sentinel at the HTTP boundary |

**Observation:** Three runner POST endpoints each handle "the receiver hasn't persisted this run yet" but assert on different sentinels:

```go
// RecordTelemetry, dashboard.go:345
if errors.Is(err, store.ErrUnknownRun) { ... return 202 }

// RecordStatus, dashboard.go:407
if errors.Is(err, sql.ErrNoRows) { ... return 202 }

// RecordHeartbeat, dashboard.go:557
if errors.Is(err, sql.ErrNoRows) { ... return 202 }
```

The store package exports `store.ErrUnknownRun = errors.New("store: unknown run")` (defined in `store/telemetry.go:24`) as the canonical "I don't know that run id" sentinel. `RecordTelemetry` uses it. `RecordStatus` and `RecordHeartbeat` instead check the raw `database/sql` sentinel from the underlying `QueryRowContext` path.

**Impact:** The two paths are functionally equivalent today (a missing row surfaces as `sql.ErrNoRows` from `QueryRow`, and `RecordTelemetry` wraps that as `ErrUnknownRun`), but the divergence is at the *handler* layer, where the choice of sentinel is meant to be stable. A future change to `UpdateRunStatus` (in `runs.go`) that wraps its `ErrNoRows` as `store.ErrUnknownRun` for symmetry will keep `RecordStatus`'s check working *only* if it knows the wrapper exists. Today, the asymmetry is a smell; after a future refactor, it is a bug.

There is a third reader-hazard: `RecordStatus` and `RecordHeartbeat` use the same `sql.ErrNoRows` shape, but the *body* of the 202 response differs (no body in either case, but the doc comments at `dashboard.go:407-411` and `:541-545` differ in tone — one says "the runner will retry on the next stage transition", the other says "the runner will retry on the next tick"). A reader looking at why two endpoints do the same thing differently will check the comments first and find a useful distinction (stage-transition vs. tick), then wonder why the code does not reflect it.

**Suggestion:** Pick one sentinel and use it everywhere. `store.ErrUnknownRun` is the right one — it is exported, has a doc comment, and the test file (`store_test.go:312, 791`) pins it as the contract. Make `UpdateRunStatus` and `TouchRunHeartbeat` wrap their `sql.ErrNoRows` paths in `ErrUnknownRun`, then change `RecordStatus` and `RecordHeartbeat` to `errors.Is(err, store.ErrUnknownRun)`. The diff is small (two line changes on the store side, two line changes on the handler side), and the consolidation lets a reader grep `ErrUnknownRun` to find every "unknown run" branch.

Defer: the asymmetry is dormant (no bug today) and the change touches the store's behaviour, which is wider than the dashboard PR.

---

### RD-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/dashboard/dashboard.go:142-146` — `if s := q.Get("limit"); s != "" { _ = s }` dead branch |

**Observation:** The runs-list view has a placeholder for the `limit` query string that does nothing:

```go
if s := q.Get("limit"); s != "" {
    // The default (50) is fine for a dashboard page;
    // no override needed.
    _ = s
}
```

The block parses the query, says the default is fine, then discards the value. The next line calls `h.store.ListRuns(r.Context(), f)` with `f.Limit` still zero (the default). The comment is honest about what is going on; the code is also honest (the `_ = s` makes the discard explicit).

**Impact:** A reader skimming the function for "what query parameters does this view accept?" sees `limit` in the form (good) and wonders why it has no effect. The answer is "the dashboard hard-codes 50" but the *code* is what they have to read. The block also sets a precedent: a future dashboard view that needs `limit` to round-trip will copy the pattern and forget the `_ = s`. The structural audit's `CQ-004` is the obvious partner here — the query-string key advertises a feature that the code does not implement.

**Suggestion:** Two options:

1. Drop the block entirely. The comment about "the default is fine" can move to the runs.html template (where the form is rendered — see `runs.html:5-22`, which does not currently include a `limit` field, so the URL parameter is operator-typed and undocumented in the UI).
2. Wire the limit through. The `ListRunsFilter` already has a `Limit` field; the handler just needs to parse and clamp.

Option 2 is the one a future author will want anyway (the dashboard's runs list will eventually paginate), but Option 1 is the right "delete the dead code now, add it back when there is a UI" move. The PR ships Option 1 with no UI change; the limit can land alongside the pagination control.

---

### RD-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/dashboard/dashboard.go:107` and `apps/receiver/internal/webhook/dashboard.go:225-247` — magic numbers for time windows without a named constant |

**Observation:** Three time-window magic numbers appear without a named constant, in two files:

- `dashboard/dashboard.go:309` — `h.store.ListStuckRuns(r.Context(), 2*time.Minute, 100)` (the "stuck" threshold)
- `dashboard/dashboard.go:358` — `h.store.ListRuns(r.Context(), store.ListRunsFilter{Status: store.StatusFailed, Limit: 200})` (exception dock's window)
- `dashboard/dashboard.go:400` — `to := time.Now().UTC(); from := to.Add(-30 * 24 * time.Hour)` (costs view's 30-day window)
- `webhook/dashboard.go:223` — same 30-day window, hard-coded again
- `webhook/dashboard.go:263` — `h.store.PerRepo(ctx, from, to, 50)` (top 50 repos)
- `k8s_reconcile.go:129-132` — `30 * time.Second` default + `5 * time.Second` floor for the reconciler poll

The 30-day window is in two files; the "stuck = 2 minutes" rule is named in the `ListStuckRuns` docstring (`stages.go:147-150`: "a 2-minute gap with status=running = stuck") and again in the runner comment (`dashboard.mjs:104-107`: "no heartbeat in 2 minutes while status=running = 'stuck'") but the literal `2*time.Minute` is only in one place. The other copy is the English phrase "2 minutes" in a comment.

The `k8s_reconcile.go:114-115` comment explains the *why* ("5s floor — a tight poll against the K8s API is wasteful") but the literal is the only place the value lives.

**Impact:** Today the magic numbers are few and well-commented. The cost shows up the next time the rule changes. The runner's stuck-detection comment ("no heartbeat in 2 minutes") and the receiver's `2*time.Minute` literal have to be edited in lockstep. The same applies to the 30-day window (the costs view and the stats endpoint both default to 30 days). A reader who arrives at the dashboard to change the stuck threshold to 3 minutes finds the literal at one site, the English phrase at three, and the test file at... (none — the existing tests do not pin the threshold).

**Suggestion:** One small named-constants block in each package. For the dashboard:

```go
// apps/receiver/internal/dashboard/dashboard.go
const (
    stuckRunThreshold = 2 * time.Minute
    exceptionsLimit   = 200
    costsDefaultFrom  = 30 * 24 * time.Hour
)
```

And for the webhook (the 30-day window and the top-50 repos default):

```go
// apps/receiver/internal/webhook/dashboard.go
const (
    statsDefaultFrom = 30 * 24 * time.Hour
    statsTopRepos    = 50
)
```

For `k8s_reconcile.go`:

```go
const (
    reconcilerDefaultInterval = 30 * time.Second
    reconcilerMinInterval     = 5 * time.Second
)
```

Each is a 3-line change. The value is the same; the next reader's misread cost is bounded.

---

### RD-006
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/dashboard/dashboard.go:96-99` — `RegisterRoutes` matches `GET /dashboard/` and `POST /dashboard/` to the same handler |

**Observation:** The route registration is:

```go
mux.Handle("GET /dashboard/",  h.Middleware(http.HandlerFunc(h.route)))
mux.Handle("POST /dashboard/", h.Middleware(http.HandlerFunc(h.route)))
```

The single `h.route` function then dispatches on `r.URL.Path` for every method. The route table at `dashboard.go:109-129` ignores the HTTP method — every path branch is method-agnostic. The exception is `serveInstallationControl` (path `"installations/<id>"`) which reads the POST body via `r.ParseForm()` (`:449`) and writes via `http.Redirect` (`:467`); a GET to the same path will execute the form-parsing path with an empty body and end up at `h.store.SetInstallationControls(ctx, id, false, nil)` because `paused := r.FormValue("paused") == "true"` defaults to `false`.

**Impact:** A GET to `/dashboard/installations/123` silently pauses nothing (the form values are empty), but it also silently returns a 303 redirect to `/dashboard/installations` and writes the row with `paused=false, lens_opt_out=nil`. The cost is small (false and nil are the safe defaults) but the verb-blind dispatch is a footgun. The same problem does not exist for any other dashboard route (the rest are GETs and ignore the body), but a future POST endpoint added to this handler will have to remember to check the method at the top of its branch.

**Suggestion:** Either (a) drop the POST registration entirely if no POST endpoints land in this handler (the re-run POST endpoints live in `webhook/rerun.go`, the install-control POST is the only dashboard POST and it is a one-off); or (b) keep the POST registration but make `h.route` check `r.Method` at the top of each branch that writes. Option (a) is the simpler change — the install-control POST can be wired with `mux.HandleFunc("POST /dashboard/installations/{id}", h.Middleware(http.HandlerFunc(h.serveInstallationControl)))` and the rest of the dispatcher becomes a GET-only path table.

Defer: this is a footgun-shape finding, not a current bug.

---

### RD-007
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/dashboard/dashboard.go:496-497` — `var _ = context.Background` placeholder, sibling to RD-001 |

**Observation:** The dashboard file ends with:

```go
// ensure context import survives goimports.
var _ = context.Background
```

This is a defensive placeholder so a future maintainer who adds a function that uses `context` does not get the goimports drop. The `context` import is *not* used in the file as it stands — the file's `r.Context()` calls all go through `http.Request`, which does not require a `context` import. The comment is honest about the intent.

**Impact:** This is the milder sibling of RD-001. The pattern (`var _ = something` to keep an import alive) is a known Go convention; the comment explains it. A reader skimming the file bottom for "what does this `context` import do?" finds the line and the comment and walks away knowing "nothing, yet".

The cost is one line. The benefit is the next maintainer who adds a context-aware feature does not have to re-add the import. Both are real, but small.

**Suggestion:** Leave as-is. The pattern is the codebase's own convention (the same file pattern appears in `webhook/crash_safety_test.go:484` for `json.Marshal`), the comment is short, and the alternative (drop the import and re-add it on demand) costs the next maintainer one keystroke but is otherwise equivalent. Flagging for completeness — this is the kind of placeholder the agent spec calls out as "suspicious-looking", but in this codebase the pattern is load-bearing for the dev experience.

---

### RD-008
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/dashboard/dashboard.go:443-445` — `serveInstallationControl` parses the int with `fmt.Sscanf` |

**Observation:** The install-control handler parses the URL id with:

```go
var id int64
if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
    http.Error(w, "bad id", http.StatusBadRequest)
    return
}
```

`fmt.Sscanf` is the slow, format-string-driven scanner. The other handler in the same file (`webhook/dashboard.go:140-141`) parses the same kind of value with `strconv.ParseInt`:

```go
if n, err := strconv.ParseInt(s, 10, 64); err == nil {
    f.InstallationID = n
}
```

The dashboard's other int parse path is `webhook/dashboard.go:155` (`strconv.Atoi`). `fmt.Sscanf` does not appear anywhere else in the new dashboard code.

**Impact:** The `fmt.Sscanf` choice is the lone exception — every other int-from-string parse in the changed files uses `strconv`. A reader who lands on `serveInstallationControl` from the `webhook/dashboard.go` int-parsing pattern will do a double-take. The cost is one re-read plus a momentary "is there a reason this one is different?" thought (there isn't — the install-control POST is the only int parse in the dashboard package, and the package does import `strconv` nowhere else, but the import is cheap).

**Suggestion:** Replace with `strconv.ParseInt` for consistency with the rest of the new code:

```go
id, err := strconv.ParseInt(idStr, 10, 64)
if err != nil {
    http.Error(w, "bad id", http.StatusBadRequest)
    return
}
```

The change is 4 lines and one new import (`strconv`). The `fmt` import on `dashboard.go:28` is also used at `:61` (`fmt.Errorf`) and `:162` (`fmt.Sprintf`), so the import stays.

---

### RD-009
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/dashboard.go:290-300, 580-591` — `telemetryRequest` and `lensTelemetryRequest` carry the same 8 fields with one word of difference |

**Observation:** Two request structs in the same file carry the same shape:

```go
// dashboard.go:290-300
type telemetryRequest struct {
    Model, Provider string
    InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens int64
    CostUSD float64
    StepCount int
}

// dashboard.go:580-591
type lensTelemetryRequest struct {
    Lens, Model, Provider string
    InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens int64
    CostUSD float64
    StepCount int
}
```

The `lensTelemetryRequest` adds one field (`Lens`); the rest of the token / cost / step fields are identical. The struct on the store side (`store.LensTelemetry` in `lens_telemetry.go:31-45`) is the same shape too.

**Impact:** The duplication is small (8 fields repeated three times across two packages) and the doc comments are clear about *why* (the wire shape mirrors the OpenCode event shape, the store shape mirrors the row). The cost is the next author who adds a new token-counter field has to touch three sites in two files. The runner side (`dashboard.mjs:51-67, 165-184`) has the same duplication in JS — the runner builds the `postTelemetry` and `postLensTelemetry` payloads from `state.review.telemetry` plus a per-lens `lenses.map(l => ...)` shape, both with the same eight numeric fields.

The structural-audit hit is the same as RD-002's: the convention is held in many places.

**Suggestion:** Hoist the shared fields into a Go embed and reuse:

```go
type tokenCostFields struct {
    Model, Provider    string
    InputTokens        int64 `json:"input_tokens"`
    OutputTokens       int64 `json:"output_tokens"`
    ReasoningTokens    int64 `json:"reasoning_tokens"`
    CacheReadTokens    int64 `json:"cache_read_tokens"`
    CacheWriteTokens   int64 `json:"cache_write_tokens"`
    CostUSD            float64 `json:"cost_usd"`
    StepCount          int    `json:"step_count"`
}

type telemetryRequest struct {
    tokenCostFields
}

type lensTelemetryRequest struct {
    Lens string `json:"lens"`
    tokenCostFields
}
```

The JSON shape is identical (the embedded fields are promoted). The store side can do the same with `store.LensTelemetry` (it does not embed today, but the embed makes the "this is the same shape" intent explicit). Defer to the next time a token-counter field is added; the cost of the duplication is bounded until then.

---

### RD-010
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/workflow.mjs:342-371` — `runSubWorkflow` reads `state._subWorkflowOf` and `state.sub[macroId]`, no encapsulation |

**Observation:** The sub-workflow executor threads its "which macro am I owned by" identity through `state._subWorkflowOf`, set by the macro stage and read by the sub-executor:

```js
// workflow.mjs:342
const macroId = state._subWorkflowOf;

// workflow.mjs:579-584
async function sniffStage(ctx, deps, overrides, state) {
  state._subWorkflowOf = "sniff";
  try {
    await runSubWorkflow(REVIEW_SUB_STAGES, ctx, deps, overrides, state);
  } finally {
    delete state._subWorkflowOf;
  }
}
```

The underscore prefix signals "this is a private protocol between `sniffStage` and `runSubWorkflow`." The protocol is also string-typed — `state._subWorkflowOf = "sniff"` is the only assignment, but a future macro stage that wraps a sub-workflow will write a different string. The shape `state.sub[macroId]` is also stringly-keyed (the key is the macro id).

**Impact:** The convention works because there is exactly one macro that wraps a sub-workflow (`sniff`). The cost shows up the day a second macro needs to do the same. The macro id `"sniff"` is repeated in three places: the assignment at `:579`, the `dispatchSubStage` log context, and the test fixture at `workflow.test.mjs:810-1185` (the tests re-set it before each `runSubWorkflow` call). A typo (`"snif"`) is a silent failure mode — `state.sub["snif"]` is a fresh empty object, and the sub-workflow runs as if it were a fresh start.

**Suggestion:** Pass the macro id as a parameter, not as a `state` field:

```js
export async function runSubWorkflow(stages, ctx, deps, overrides, state, macroId) {
    const subPassed = macroId && state.sub?.[macroId] || null;
    // ...
    if (macroId) {
        state.sub = state.sub || {};
        state.sub[macroId] = state.sub[macroId] || [];
        if (!state.sub[macroId].includes(stage.id)) {
            state.sub[macroId].push(stage.id);
        }
    }
}

// caller
async function sniffStage(ctx, deps, overrides, state) {
    await runSubWorkflow(REVIEW_SUB_STAGES, ctx, deps, overrides, state, "sniff");
}
```

The `state._subWorkflowOf` set/delete dance goes away; the test fixtures drop the manual set + delete + the cleanup-via-finally. The string `"sniff"` lives in one place (the `sniffStage` call) and the executor's parameter signature pins it.

Defer — the cost is bounded (one macro today) and the change touches 30+ test fixture lines.

---

### RD-011
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/dashboard.mjs:194-225` — `postWithRetry` retry policy is two module-level constants and a magic multiplier |

**Observation:** The retry helper is:

```js
const POST_TIMEOUT_MS = 5000;
const POST_RETRIES = 1;

async function postWithRetry(url, body, token, deps) {
  for (let attempt = 0; attempt <= POST_RETRIES; attempt++) {
    // ...
    if (attempt < POST_RETRIES) {
      await sleep(200 * (attempt + 1));
    }
  }
}
```

Three numbers control the policy: `POST_TIMEOUT_MS` (5s per call), `POST_RETRIES` (one retry), and the backoff (`200ms * (attempt + 1)` — so 200ms, no doubling, capped at 200ms because there is only one retry). The `POST_RETRIES` constant is named like a count but used in a `<=` loop, so the actual number of attempts is `POST_RETRIES + 1` (two on the wire, not one).

**Impact:** A reader looking at `POST_RETRIES = 1` and the loop bound `attempt <= POST_RETRIES` has to mentally add one to get the "two total attempts" number. The backoff `200 * (attempt + 1)` is a linear ramp inside a loop that runs at most once, so the comment in `dashboard.mjs:138-142` ("a 30s first tick still gets two more attempts before stuck lights up") is the only place the operational meaning of "two attempts" is written down.

The cost is one re-read at the retry helper. The bigger issue is the helper is `internal-only` (it is not exported and not reused outside `dashboard.mjs`) so the constants are the right scope, but the loop's "≤" is one more thing to notice.

**Suggestion:** Either rename the constant to `POST_MAX_ATTEMPTS = 2` and use `< POST_MAX_ATTEMPTS`, or comment the loop bound. The other `apps/runner` files use the same `<= POST_RETRIES` shape (see `opencode.mjs`'s retry helpers) so the rename is a small refactor. Lower-priority than RD-001 / RD-002 / RD-003 — flagging because the agent spec called out "magic values" and this is the retry helper's.

---

### RD-012
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/dashboard.go:51-108` and `apps/receiver/internal/dashboard/dashboard.go:96-130` — `ListInstallations` and `serveInstallations` are two ways to spell the same view |

**Observation:** Two files both render the installations list with overlapping logic:

- `webhook/dashboard.go:51-108` — `ListInstallations` is the JSON API for `/api/installations`. It hits `h.store.ListInstallations`, does a cold-start synchronous refresh if the cache is empty, then converts the store rows to `boopgithub.Installation` shape and emits JSON.
- `dashboard/dashboard.go:414-427` — `serveInstallations` is the HTML view for `/dashboard/installations`. It hits `h.store.ListInstallations` (no cold-start refresh) and renders the rows in the template.

The two views diverge on three points:
1. The JSON endpoint cold-start refreshes; the HTML view does not. If the receiver restarts and the operator lands on `/dashboard/installations` in the first 5 minutes, they see an empty list. The JSON path handles this.
2. The JSON endpoint carries a `fetched_at` timestamp; the HTML view does not.
3. The JSON endpoint exposes the same row shape as the store (`boopgithub.Installation`); the HTML view adds `LensOptOutCSV`.

The conversion from `store.Installation` to `boopgithub.Installation` happens twice (`webhook/dashboard.go:71-79` and `:94-102`), with the same 5 fields each time. The cold-start refresh path also reads the same fields and writes them back via `UpsertInstallations`.

**Impact:** The duplicated conversion is the same pattern as RD-009's: small, structural, and the next field added to `store.Installation` will need to be added in two places (plus the upsert at `webhook/dashboard.go:704-712`). The "JSON cold-starts, HTML does not" divergence is a real bug-shape: the operator's `/dashboard/installations` view can be empty right after a receiver restart, even though the JSON path next to it is populated.

**Suggestion:** Centralize the cold-start behaviour in a single store method (or a single webhook helper) that both views call. The cleanest is a `store.ListInstallationsWithColdStart(ctx, ghClient)` that returns the rows (refreshing if empty). The JSON handler and the HTML handler both call it; the JSON handler additionally asks for `fetched_at` and the HTML handler adds the `LensOptOutCSV` column.

The store-side change is wider than the dashboard PR (it would live in `store/installations.go` next to the existing `ListInstallations`), so defer. The duplication is bounded until a new field is added to `store.Installation`.

---

### RD-013
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/webhook/dashboard.go:286-300, 467-471, 580-591` — three `*Request` docstrings describe the same "runner posts, receiver stamps" pattern with three different phrasings |

**Observation:** Three POST request types (`telemetryRequest`, `stageRequest`, `lensTelemetryRequest`) each have a docstring that re-explains "the runner posts at end-of-run / on stage transition, the receiver stamps with its own clock, REPLACE any existing row, etc." The shapes are similar but the docstring lengths diverge:

- `telemetryRequest` (`:286-300`): 4 lines about the OpenCode event-shape mirror
- `stageRequest` (`:456-471`): 11 lines about the "one clock" rule and the `client_started_at` ignore
- `lensTelemetryRequest` (`:568-591`): 9 lines about the `lens: <name>` markers and the REPLACE

The repetition is not bad — the docstrings are well-targeted at each type's quirks. The cost is a reader who arrives at the file bottom (after `RecordStage`, after `RecordLensTelemetry`) and wonders if the three "REPLACE any existing row" comments describe different semantics. They do not (they describe the same `INSERT OR REPLACE` / `ON CONFLICT DO UPDATE` pattern at the store layer), but the docstrings do not cross-reference each other.

**Impact:** A reader who scans the file looking for "what's the contract?" has to re-read the same paragraph three times. Each re-read is 30 seconds, so 90 seconds total to confirm the contracts are the same. The cost is small but real.

**Suggestion:** Add a one-line cross-reference at the top of the file:

```go
// package doc: All three "runner POST" endpoints (telemetry, stage, lens_telemetry)
// follow the same contract: the runner sends the names of what changed, the
// receiver stamps with its own clock, and re-deliveries are idempotent. The
// per-endpoint docs below describe only what is unique to that endpoint.
```

The per-endpoint docs stay (the "one clock" rule on `stageRequest` is unique and load-bearing), but the duplication is bounded by the cross-reference. Two lines at the file top; no per-function change.

---

### RD-014
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/workflow.mjs:294-306` — `runStages` failure-reason string is a templated sentence that mentions the full passed set |

**Observation:** The duplicate-pod abort path constructs the failure reason in `runStages`:

```js
const reason = `another pod already passed [${state.passed.join(", ")}]; refusing to duplicate the review`;
deps.errlog("abort", reason, {
    stage: stage.id,
    passed: state.passed,
});
```

The string carries two pieces of information: (a) "another pod beat us", (b) the full list of stages the prior pod passed. The `state.passed` array is the actual data; the reason string is the human rendering. The `deps.errlog` already includes `passed: state.passed` as a structured field, so the data is captured twice (once in the string, once in the log fields).

**Impact:** The string is what the operator reads on the dashboard. The structured field is what an alerting pipeline would consume. Both are useful. The duplication is intentional (the human-readable string is a UX win on the dashboard's "failed" row), but a future log-search for "another pod already passed" will surface the string but not the structured array (and vice versa). A reader arriving at the code from the dashboard side sees the string and is happy; a reader arriving from the log side sees the same string and is also happy.

The cost is zero today. The risk is that a future change to the reason format (e.g. "duplicate of {primary_run_id} which passed {stages}") forgets to update one of the two surfaces and they drift.

**Suggestion:** Leave as-is. The redundancy is intentional and the two surfaces target different consumers (human vs. log query). Flagging because the agent spec called out "suspicious-looking variables" and the string templating is the obvious candidate.

---

### RD-015
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/dashboard/dashboard.go:135-146` — `serveRuns` constructs a `runsFilter` from query params that includes `q`, `status`, and `failure_class` — but only `status` and `failure_class` are wired to the store filter |

**Observation:** The runs view reads three query parameters:

```go
f := store.ListRunsFilter{
    Owner: q.Get("owner"),
    Repo:  q.Get("repo"),
}
if s := q.Get("status"); s != "" {
    f.Status = store.RunStatus(s)
}
if s := q.Get("limit"); s != "" { _ = s } // RD-004
// ...
data := runsView{
    // ...
    Filter: runsFilter{Q: r.URL.Query().Get("q"), Status: q.Get("status"), FailureClass: q.Get("q")},
}
```

The `runsFilter` view-model includes `Q` (the "owner/repo or PR#" search field) but the store filter only carries `Owner` and `Repo` (not `Q`). The HTML form (`runs.html:6`) posts `q` and the value is echoed back into the form input — it is never used by the store. The dashboard effectively has a "search" input that is cosmetic.

The `failure_class` filter is also echoed back into the view-model (`q.Get("failure_class")` in the `runsFilter` init at `:169`), but the store does not filter on `failure_class` either. The exception dock's view at `dashboard.go:356-380` *does* filter on `class` (in-memory, post-fetch), but the runs view does not.

**Impact:** A reader who lands on the runs view from a fresh-tab URL like `/dashboard/runs?q=michaelruelas/boop-receiver&failure_class=oom_killed` sees a non-filtered list (only `status` and the URL filters the *store* knows about are applied). The form will repopulate with the same `q` and `failure_class` values, so the URL *looks* like it filtered, but the rendered table is the unfiltered set.

This is the same shape as RD-004 — a form field is advertised but the backend does not honour it. The two are siblings; the difference is that `limit` is in the store's `ListRunsFilter` (RD-004's `f.Limit` field exists), whereas `q` and `failure_class` are not even on the store-side struct.

**Suggestion:** Either drop `q` and `failure_class` from the form (so the URL state matches the rendered state), or add `FailureClass` to `store.ListRunsFilter` and parse `q` into a `(owner, repo)` or `pr_number` shape. The dropdown for `failure_class` in `runs.html:14-20` is the more useful one to wire through (the run rows carry a `failure_class` column, so a `WHERE failure_class = ?` is one line). The `q` field is a UX nicety and can wait.

Defer: the feature is not yet implemented and the form is a placeholder. Flag for the next pass on the runs view.

---

## Naming Conventions Observed

Derived from the dashboard cross-cutting code. The Go side has converged on a clear convention; the Node side has a small but consistent set; the cross-cutting "header auth" pattern is the convention worth pinning.

### Shared

| Element              | Convention                          | Example                                              |
|----------------------|-------------------------------------|------------------------------------------------------|
| File names           | kebab-case for Go, kebab-case for Node | `dashboard.go`, `k8s_reconcile.go`, `dashboard.mjs` |
| Sentinel errors      | `Err<Subject>` exported             | `store.ErrUnknownRun`                                |
| Per-action log keys  | kebab-case (`run`, `stage`, `err`)  | `h.logger.Warn("record status", "run", id, "err", err)` |
| Time-window constants | unexported `lowerCamel` + noun      | (none yet — see RD-005)                              |
| Suffix on duplicated helpers | `<Thing>Rerun` / `<Thing>Rerun` suffix when crossing package or copying an unexported helper | `shortSHARerun`, `buildJobNameRerun`, `rerunJobNameSanitizer` |
| Stage names          | kebab-case in the database, kebab-case in the runner | `hmac_verify`, `pod_schedule`, `comment_post` |
| Failure classes      | snake_case, prefixed by category    | `oom_killed`, `container_error`, `crash_loop`, `image_pull` |
| Status string values | lowercase (the `RunStatus` constants) | `pending`, `running`, `succeeded`, `failed`        |
| "One clock" rule     | receiver stamps started_at / ended_at / heartbeat | `RecordStage` (`dashboard.go:507-512`), `TouchRunHeartbeat` (`stages.go:128-130`) |
| "Replace on re-deliver" | DELETE + INSERT or `ON CONFLICT DO UPDATE`; docstring mentions the idempotency contract | `ReplaceLensTelemetry` (`lens_telemetry.go:47-57`), `UpsertRunStage` (`stages.go:32-44`) |

### Go side (`apps/receiver`)

| Element              | Convention                          | Example                                              |
|----------------------|-------------------------------------|------------------------------------------------------|
| Variables            | mixedCaps, lower-case initial       | `stuckThreshold`, `pendingJobs`, `pollInterval`      |
| Functions            | mixedCaps, verb + noun              | `ListRuns`, `RecordTelemetry`, `failureClassFromContainerState` |
| Public types         | PascalCase                          | `RunStatus`, `LensTelemetry`, `RetentionRow`         |
| Unexported view-model structs | lowerCamel + suffix `View` / `Row` / `Filter` | `runsView`, `runsRow`, `runsFilter`, `exceptionRow` |
| Struct embedding for view-models | embed `store.Run` and override the formatted field | `runsRow` (`dashboard.go:186-192`)         |
| SQL time format      | `time.RFC3339Nano` round-trip       | `started_at`, `occurred_at`, `recorded_at` columns  |
| SQL nullable handling | `COALESCE(col, '')` on read; `nullString` / `nullTimePtr` / `nullInt64Ptr` on write | `audit.go:64-65`, `stages.go:57-59`                  |
| HTTP status code convention | 200 / 202 / 204 / 400 / 401 / 404 / 409 / 500 / 503 | "202 = transient (retry), 204 = landed, 409 = wrong state" |
| Auth header naming   | `X-<Product>-<Token>`               | `X-Boop-Dashboard-Token`, `X-BOOP-Runner-Token`      |
| Loop / scan variable names | `i`, `ev`, `lt`, `r`, `ins`, `s` (single letter OK in tight loops; descriptive at the call site) | `for i := range jobs.Items` (`k8s_reconcile.go:167`) |
| Time-handling helpers | inlined `time.Now().UTC()` in writes, `time.Parse(time.RFC3339Nano, ...)` in reads | `stages.go:52-56`, `audit.go:109-111`                |
| Defensive `var _` placeholders | admitted in the comment, kept for goimports | `var _ = context.Background` (`dashboard.go:496-497`), `var _ = json.Marshal` (`crash_safety_test.go:484`), `var _ sql.NullString` (`audit.go:195-200` — see RD-001) |
| Job name convention  | `boop-<owner>-<repo>-<pr>-<sha7>` (original) and `...-<sha7>-r<n>` (re-run) | `buildJobName` (`handler.go:1145-1147`), `buildRerunJobName` (`rerun.go:235-240`) |
| Failure-class vocabulary | mapped to lowercase snake_case in `mapExitReason` / `mapWaitingReason` | `k8s_reconcile.go:64-103`                           |
| Boolean flag in struct field | noun (no `is` prefix) on domain structs, predicate form on locals | `FailureClass string`, `Paused bool` (on `Installation`); `paused := r.FormValue("paused") == "true"` (local) |

### Node side (`apps/runner`)

| Element              | Convention                          | Example                                              |
|----------------------|-------------------------------------|------------------------------------------------------|
| Variables            | camelCase                           | `telemetry`, `lenses`, `subPassed`                   |
| Functions            | camelCase, verb + noun              | `postStatus`, `postTelemetry`, `startHeartbeat`      |
| Constants (module-level) | UPPER_SNAKE_CASE              | `POST_TIMEOUT_MS`, `POST_RETRIES`, `DEFAULT_MAX_ATTEMPTS` |
| Exported functions   | `export function` / `export async function` | `runStages`, `postStage`, `statusStageFor`     |
| Internal helpers     | lowercase, not exported             | `jobNameFromCtx`, `postWithRetry`, `currentRunOrEmpty` |
| Placeholder state keys | `_`-prefix on temporary protocol fields | `state._subWorkflowOf` (`workflow.mjs:579`)    |
| Sub-workflow id      | macro-stage id (string), e.g. `"sniff"` | `state._subWorkflowOf = "sniff"`                 |
| Retries              | `<= POST_RETRIES` loop, actual attempts = `POST_RETRIES + 1` (see RD-011) | `dashboard.mjs:196-224` |
| HTTP timeouts        | millisecond constants               | `POST_TIMEOUT_MS = 5000`                             |
| First-tick delay     | comment at the `setTimeout` / `setInterval` call, not a constant | `dashboard.mjs:137-142` (30s)                    |
| Best-effort logging  | `deps.log("subsystem", "message", { ... })` | `deps.log("dashboard", "post failed", { url, status, attempt })` |
| Error shape          | `new Error("message: <context>")` or string returned | `throw new Error("...")`                           |
| Test fixture keys    | `_`-prefix on the literal-marker fields, descriptive on the rest | `_subWorkflowOf: "sniff"` (`workflow.test.mjs:810`) |
| Sub-workflow id pinned at the call site | passed in `state._subWorkflowOf = "sniff"`, set in macro, cleared in `finally` | `workflow.mjs:579-584`                  |

### Cross-cutting

- **Auth header casing is inconsistent within the same file**: `X-BOOP-Runner-Token` (all-caps) is the convention for the data-layer POSTs, but `X-Boop-Dashboard-Token` (title-case) is the convention for the dashboard view routes (`dashboard.go:82`). The case difference is intentional per the doc comments (one is "the runner's identity token", the other is "the dashboard's secret"), but a grep for `X-BOOP` will miss the dashboard path. Worth a follow-up if a future auth refactor unifies the two — see `webhook/dashboard.go:18-19` and `dashboard/dashboard.go:74-75` for the per-endpoint rationale.
- **"Failed" reason flow**: the runner's `state.failureReason` is set in three places (`onGateFailure`, the duplicate-pod abort, the orchestrator's catch), then forwarded to the dashboard as `error` in the status payload. The dashboard renders the reason in the `failed` row. The two-package convention is spelled out in `dashboard.mjs:30-36` (the JSDoc for `postStatus`) and `webhook/dashboard.go:367-372` (the `statusRequest.Error` field).
- **Idempotent re-delivery convention**: the runner uses `INSERT OR REPLACE` / `ON CONFLICT DO UPDATE` for every state-bearing table, with a docstring that names the at-least-once delivery contract. The store side does the same with `Upsert*` method names. The `lens_telemetry.go:47-57` docstring is the canonical example of the contract being written down.
- **"Period 0 → default" pattern**: every loop helper that takes a period argument uses the convention "0 means 'use the package default'; the resolved value is logged at startup." Examples: `StartRetentionLoop` (`webhook/dashboard.go:735-780`), `StartJobReconciler` (`k8s_reconcile.go:124-152`), `StartInstallationsPoller` (`webhook/dashboard.go:667-697`). The pattern is consistent across the three; the resolved values are spelled out in the same startup log line.

---

## Unable to verify

- **Whether the `_ = sql.NullString` placeholder in `store/audit.go:200` is a deliberate guard against an upcoming `ListAuditEvents` refactor or a leftover from an earlier draft.** The comment says "future use of sql.NullString in the audit event's details column scan path", but the scan at `audit.go:106` is already written and uses bare `string` against a `COALESCE(col, '')` column. The next author who lands a nullable-`details` column is the one who would know whether this placeholder was set up for *that* future shape or whether it was left in by accident. RD-001's recommendation is to delete it; worth asking the author before merging if it has a non-obvious use.
- **Whether the runner's "first-tick delay" of 30s (`dashboard.mjs:143`) should be a constant alongside `POST_TIMEOUT_MS` and `POST_RETRIES`.** The same delay appears in `webhook/dashboard.go` (15s, three times) and `k8s_reconcile.go:137` (15s). The receiver's value is half the runner's; the rationale ("let the receiver bind its port before we start hammering") is the same. The values are close enough that they could be a single `firstTickDelay = 15 * time.Second` constant, but the runner's 30s is load-bearing against the stuck-runs panel's 2-minute threshold (the comment at `dashboard.mjs:137-142` explains). A reader looking at this today cannot tell whether the runner's 30s is a separate design decision or just a copy-paste with a different number.
- **Whether the `failureClassFromContainerState` / `mapExitReason` / `mapWaitingReason` trio (`k8s_reconcile.go:47-103`) is meant to be exported as a package-level vocabulary.** The functions are unexported and live next to the only caller (`reconcileJobsOnce`). The mappings (the "v1 spec's exception-dock filter values" per the docstring) read as if they could be reused by a future "headless classify" command or a unit test fixture. Today there is one caller, so unexported is correct; flag for the next audit that touches failure classification.
- **Whether the `tok` / `t` / `recAt` single-letter scan variables in the store files (`stages.go:282, audit.go:104, lens_telemetry.go:122`) are the codebase convention or per-author preference.** The other `Run`-scan helpers in `runs.go` use a mix of single-letter and descriptive names. The pattern is consistent within each file, but the choice of single-letter vs. descriptive seems to track the number of fields being scanned (>10 → single letter, ≤5 → descriptive). A reader landing on `stages.go:282` from a 5-field scan in `audit.go:104` will notice the difference.
- **Whether the `0` default for `BOOP_INSTALL_POLL_INTERVAL` / `BOOP_RETENTION` / `BOOP_CLEANUP_EVERY` / `BOOP_VACUUM_INTERVAL` / `BOOP_BACKUP_EVERY` / `BOOP_BACKUP_KEEP` (the `Config` struct in `handler.go:90-100`) is meant to be unified with the runner's `BOOP_STAGE_MAX_ATTEMPTS` / `BOOP_STAGE_BACKOFF_BASE_MS` / `BOOP_STAGE_BACKOFF_MAX_MS` env-var defaulting convention.** The receiver side uses `0` (a typed zero) and the runner uses named constants in `workflow.mjs:78-80`. The receiver's `Config` docstring at `handler.go:85-89` spells out the "0 = store default" rule, the runner's `STAGE` retry defaults are exported. A future config-typing PR could land the receiver's defaults as `Config` struct tags (e.g. `default:"5m"`) but the current convention is "zero means look it up" — the two languages do the same thing different ways.
