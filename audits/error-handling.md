# Error Handling Audit
**Date:** 2026-08-06
**Scope:** `feature/qub-112-dashboard-cross-cutting` — QUB-108 to QUB-112
**Files reviewed:**
- `apps/receiver/internal/webhook/handler.go` (SubmitJob, UpsertRun before createJob, claimJobSlot, dedup)
- `apps/receiver/internal/webhook/dashboard.go` (RecordStatus, RecordStage, RecordHeartbeat, RecordLensTelemetry, RecordTelemetry, ListInstallations, cold-start refresh)
- `apps/receiver/internal/webhook/k8s_reconcile.go` (reconcileJobsOnce, classifyJobExit)
- `apps/receiver/internal/webhook/rerun.go` (RerunPreview, Rerun)
- `apps/receiver/internal/store/audit.go` (RecordAuditEvent, ListAuditEvents, ListRetentionSchedule, MarshalDetails)
- `apps/receiver/internal/store/installations.go` (UpsertInstallations transaction, SetInstallationControls)
- `apps/receiver/internal/store/runs.go` (UpsertRun, UpdateRunStatus, SetSupersededBy, GetRun)
- `apps/receiver/internal/store/rerun.go` (CountRerunJobsForSHA, WalkLineage)
- `apps/receiver/internal/store/stages.go` (UpsertRunStage, TouchRunHeartbeat, ListStuckRuns)
- `apps/receiver/internal/dashboard/dashboard.go` (Middleware, route, serveRuns, serveRunDetail, serveLive, serveExceptions, serveCosts, serveInstallations, serveInstallationControl, Health)
- `apps/runner/src/lib/dashboard.mjs` (postStatus, postTelemetry, postStage, startHeartbeat, postLensTelemetry, postWithRetry)
- `apps/runner/src/index.mjs` (postStatus refactor with slots, ensureStatusComment, stopHeartbeat in finally)
- `apps/runner/src/lib/workflow.mjs` (start/end stage POSTs)

---

## Summary

The dashboard data layer is structurally sound: HTTP status codes are mostly right (200/202/204/4xx/5xx), the runner's POST helpers use the at-least-once `UNIQUE(run_id, stage)` constraint correctly, and the receiver's foreign-key cascade keeps the orphan-row surface low. **The two highest-risk gaps are the missing 202 fallback on `RecordStage` and `RecordLensTelemetry` for a not-yet-persisted run, and the dead `RecordStage` duration logic that hardcodes `duration_ms = 0` on every end POST** — both cause silent data loss in the dashboard's waterfall without surfacing as errors. The audit log API exists but no dashboard action calls it; the docstring on `Rerun` lies about that. The runner's `postStage` is awaited (blocking the workflow) even though the helper's contract says "fire-and-forget". The optimistic `CountRerunJobsForSHA` → `UpsertRun` is racy under concurrent re-runs.

---

## Findings

### EH-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/webhook/dashboard.go:484-529` — `RecordStage` |

**Observation:** `RecordStage` does `h.store.UpsertRunStage(...)` and maps every error to 500 + body "store error". The schema has `run_stages.run_id ... REFERENCES runs(id) ON DELETE CASCADE` (migrations.go:235) and `foreign_keys=on` (store_db.go:93), so a stage POST for a run that has not yet been committed by `UpsertRun` fails the INSERT with a foreign-key violation. The runner's `postWithRetry` (dashboard.mjs:195-224) retries 5xx once, then drops the call. A start POST that arrives before the receiver's `UpsertRun` commits is lost; the end POST (which always arrives later, after the row exists) lands and stamps `started_at = now` (the handler unconditionally sets it at dashboard.go:511). The waterfall renders a `<1s` bar that is actually missing data, not a real measurement. `RecordStatus` (dashboard.go:407-413) and `RecordHeartbeat` (dashboard.go:557-560) already handle the same race with `sql.ErrNoRows → 202 Accepted`; this handler is the only POST endpoint that doesn't.

**Impact:** Silent data loss on the waterfall for any run that starts a stage before the receiver's `UpsertRun` is visible. The runner logs `"dashboard post failed"` and moves on; the operator sees a clean run with a misleading short bar.

**Suggestion:** Mirror the `RecordStatus` pattern — fetch the existing row, treat a missing run as 202:
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
Same fix for EH-002 below.

---

### EH-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/webhook/dashboard.go:611-656` — `RecordLensTelemetry` |

**Observation:** `RecordLensTelemetry` calls `ReplaceLensTelemetry` (lens_telemetry.go:57), which wraps a `DELETE + INSERT` in a transaction. The `lens_telemetry.run_id` column has the same foreign key to `runs.id` (migrations.go:357) as `run_stages`. The DELETE on a non-existent run is a no-op; the INSERT fails with FK violation, the transaction rolls back, and the handler returns 500. A `postLensTelemetry` POST for a run whose row was pruned (e.g. retention ran between the runner's first heartbeat and the final batch) returns 500 and is retried once by `postWithRetry`, then dropped. The dashboard's "lens cost" view silently omits the row.

**Impact:** Cost attribution is lost for any run pruned between the runner's start and the final telemetry batch. The runner's logs say `"dashboard post failed"`; the dashboard's rollup is silently incomplete. No 202 path exists.

**Suggestion:** Same fix as EH-001 — return 202 on missing run. Either pre-check with `GetRun` or interpret the FK error as "run not yet visible" and return 202.

---

### EH-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🔴 Blocking                                        |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/webhook/dashboard.go:507-528` — `RecordStage` (duration hardcoded to 0) |

**Observation:** When `body.Ended` is true, the handler sets `dur := int64(0)` and passes it to `UpsertRunStage`. The comment justifies this with "the start was just stamped in the same second", but that is only true for an `ended=true` POST that arrives as the first POST for the stage (no prior start POST). For the normal case (start POST landed earlier, then end POST lands later), the duration is overwritten to 0 by the `ON CONFLICT ... SET duration_ms = excluded.duration_ms` clause (stages.go:66) — `COALESCE(excluded.duration_ms, run_stages.duration_ms)` takes 0 because 0 is not NULL. The dashboard's `durMS` (dashboard.go:297-305) returns 0 because `DurationMS` is non-nil, never falling through to the `EndedAt - StartedAt` fallback. The waterfall shows a `<1s` bar with `duration_ms = 0` for a stage that actually took minutes.

**Impact:** The waterfall's per-stage durations are systematically wrong for any stage longer than 1 second. The operator's only signal that a stage was slow is the `StartedAt`/`EndedAt` timestamps in the row, which the waterfall does not surface.

**Suggestion:** Don't set `DurationMS` from the handler; let the dashboard compute it:
```go
if body.Ended {
    stage.EndedAt = &now
    // leave DurationMS nil — the dashboard's durMS helper
    // computes from EndedAt - StartedAt when DurationMS is unset.
}
```
The SQL `COALESCE(excluded.duration_ms, run_stages.duration_ms)` already preserves the existing value, so a re-delivery with a new `EndedAt` (rare) would still need the NULL treatment to keep the computation correct.

---

### EH-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Change now                                         |
| Location | `apps/runner/src/lib/workflow.mjs:314` — `await postStage(stage.id, ctx, deps)` |

**Observation:** The helper's docstring (dashboard.mjs:84-90) explicitly says: *"QUB-109 calls are fire-and-forget by design — the runner posts the START of a stage, then immediately fires the async work."* The implementation in `workflow.mjs:314` awaits the start POST, blocking the stage on every receiver round-trip. `postWithRetry` is `5s timeout + 1 retry + 200ms backoff = 10.2s` worst case. A receiver that is slow (but reachable) doubles the per-stage latency; a receiver that is dead for >2 attempts delays the stage by 10s before the helper gives up. The end POST is correctly awaited in the `finally` block, but the start is unnecessarily serial.

**Impact:** Per-run latency grows by 10s × (number of stages with failed/receiver-down starts). For a 5-stage run on a degraded receiver, that's 50s of unnecessary blocking. The "fire-and-forget" comment is aspirational; the code does not match it.

**Suggestion:** Drop the `await` on the start POST; only await the end:
```js
// start: fire-and-forget per the helper's contract
postStage(stage.id, ctx, deps);  // no await
const wasPassed = state.parseFailed;
try {
  await withRetry(stage, ctx, deps, overrides, state);
} finally {
  // end: must be observed so the operator sees the stage close
  await postStage(stage.id, ctx, deps, { ended: true });
}
```
If a refactor needs to keep ordering strict (e.g. so the waterfall shows the start before the work), use a 100ms `setTimeout` instead of `await`.

---

### EH-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/webhook/rerun.go:150-230` — `Rerun` (no transaction wrapping UpsertRun + SetSupersededBy) |

**Observation:** `Rerun` calls `UpsertRun` (inserts the new row with `ParentRunID`) and then `SetSupersededBy` (writes the inverse pointer on the prior). These are two separate transactions. If the receiver dies between them — graceful shutdown, panic, OOM — the new row lands with `parent_run_id` set but the prior row's `superseded_by_id` remains empty. The dashboard's `WalkLineage` from the prior (rerun.go:131) walks the `parent_run_id` chain forward, but the *inverse* direction (the prior → new timeline pill) is gone until something backfills it. The doc comment in `SetSupersededBy` (runs.go:208-211) acknowledges this as a non-fatal no-op for pruning, but it is silently lossy for process-death too.

**Impact:** A mid-write crash leaves an asymmetric lineage. The "vertical timeline" view from the new run still works; the prior run's row no longer shows the "superseded by" pill. The audit log (which doesn't get called anyway — see EH-006) wouldn't capture the orphaned operation.

**Suggestion:** Wrap both writes in a single transaction:
```go
tx, err := h.store.BeginTx(ctx)
if err != nil { ... }
defer tx.Rollback()
if _, err := tx.UpsertRun(ctx, newRun); err != nil { ... }
if err := tx.SetSupersededBy(ctx, prior.ID, newName); err != nil { ... }
return tx.Commit()
```
Requires adding `*sql.Tx` overloads to `UpsertRun` and `SetSupersededBy`, or a new `Store.RerunWithLineage` method.

---

### EH-006
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Change now                                         |
| Location | `apps/receiver/internal/webhook/rerun.go:135-136` (docstring) vs. `Rerun` (no `RecordAuditEvent` call) |

**Observation:** The `Rerun` docstring claims: *"`reason` is a free-form string the operator fills in; it's logged in the audit trail and surfaced on the new run's row."* The implementation calls neither `RecordAuditEvent` nor any audit-writing helper. The same is true of `serveInstallationControl` in dashboard.go:443-468 — pausing an installation or editing `lens_opt_out` produces no audit row, despite the audit_events table being the headline deliverable of QUB-112. An operator who pauses a noisy install cannot answer "who did this and when" from the dashboard; the only signal is the `installed_at`/`fetched_at` columns on the row, which don't track this surface.

**Impact:** The audit table is write-only in tests (`store_test.go:830-869`) and read-only in production. Every dashboard-mediated action taken today is unattributable. Compliance review will fail. The hard error guard in `RecordAuditEvent` (audit.go:51-56) — `empty actor` / `empty action` — means the caller has to forward the actor; the BOOP_DASHBOARD_TOKEN bearer is a stand-in until per-user identity lands, but the forwarding wiring is missing on every handler.

**Suggestion:** Add an `actor` resolver to the dashboard middleware (the BOOP_DASHBOARD_TOKEN bearer is the actor today) and call `h.store.RecordAuditEvent` in `Rerun` and `serveInstallationControl`:
```go
h.store.RecordAuditEvent(ctx, store.AuditEvent{
    Action:   "rerun.create",
    Actor:    actorFromCtx(r),
    TargetID: newName,
    Details:  store.MarshalDetails(map[string]any{"prior": prior.ID, "reason": body.Reason}),
})
```
Pair with a handler-level `defer` so a failed audit write doesn't roll back the action — log at Warn and continue. The dashboard's audit-trail view then needs a list endpoint, which `ListAuditEvents` already provides but is not yet wired to a route.

---

### EH-007
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/runner/src/lib/dashboard.mjs:212-220` — `postWithRetry` 4xx handling collapses 401 into "post rejected" |

**Observation:** `postWithRetry` treats every non-5xx/non-202 response as not-retryable and logs it as `"post rejected"`. A 401 (token mismatch) and a 400 (malformed JSON) get the same log line and no retry. 401 is a misconfiguration — `BOOP_DASHBOARD_TOKEN` on the runner doesn't match `BOOP_RUNNER_TOKEN` on the receiver — that needs operator attention, not a quiet "rejected" line. A misconfigured secret is the highest-leverage dashboard failure: every POST silently drops, the waterfall never populates, the operator only notices when something feels wrong hours later. The current logging makes the failure invisible.

**Impact:** Token-misconfiguration failures look identical to a single bad payload. The only signal that something is wrong is the absence of dashboard data, with no breadcrumb pointing to the auth path.

**Suggestion:** Log 401 at `Error` level with a breadcrumb that names the misconfiguration:
```js
if (res.status === 401) {
  deps.log("dashboard", "unauthorized (check BOOP_DASHBOARD_TOKEN vs BOOP_RUNNER_TOKEN)", { url, status: 401 });
  return;
}
if (res.status < 500 && res.status !== 202) {
  deps.log("dashboard", "post rejected", { url, status: res.status });
  return;
}
```

---

### EH-008
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/rerun.go:194-218` — count-then-upsert race |

**Observation:** `Rerun` calls `CountRerunJobsForSHA`, computes `newName = ...-r{count+1}`, then `UpsertRun(newName)`. The schema has no UNIQUE constraint on `runs.id` race-protection beyond the primary key (which is the name itself). Two concurrent re-run requests for the same `(owner, repo, pr, sha7)` tuple — e.g. an operator double-clicking the confirm button, or two operators in two tabs — both compute the same `newName`. The second `UpsertRun` hits `ON CONFLICT(id) DO UPDATE` (runs.go:103-111) and overwrites the first request's `reason` and `parent_run_id` with the second request's values. The first request's response says 202 with `new_run_id = ...-r1`; the actual row on disk has the second request's data. The lineage view then shows the second request's reason on a row that "should" be the first's.

**Impact:** Low-frequency (concurrent re-runs are rare), but when it happens the data is silently wrong. The first request's caller has no way to know their write was clobbered.

**Suggestion:** Either (a) serialize re-runs for the same `(owner, repo, pr, sha7)` tuple with a short-lived mutex or advisory lock, or (b) re-count inside the same transaction and retry the upsert with `count+1` until it lands. The simplest fix: include the re-run attempt's microsecond timestamp in `newName` so concurrent calls get distinct rows. A future cleanup can fold them.

---

### EH-009
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/rerun.go:224-229` — `Rerun` returns 202 + permanent `note` field |

**Observation:** `Rerun` returns `http.StatusAccepted` with a body that includes a `note` field: *"Phase 3 ships the lineage half; K8s Job creation is wired in Phase 4"*. The `note` is permanent API surface; a future Phase 4 PR must remember to remove it. A 201 Created would be the correct semantic (the resource is created) — 202 implies the work is accepted but not done, which is true today (no K8s Job) but false after Phase 4 ships. A dashboard consuming this today will hard-code 202 in its success handler; a future change to 201 requires touching both sides.

**Impact:** API contract drift. The `note` field becomes a deprecated field on a clean Phase 4 cutover; consumers have to know to ignore it.

**Suggestion:** Return 201 Created today with no `note`. The docstring already explains the Phase 3 vs. Phase 4 split; the body doesn't need to repeat it. When Phase 4 lands, the response is the same shape and the contract is stable.

---

### EH-010
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/dashboard.go:51-108` — `ListInstallations` cold-start refresh swallows errors silently |

**Observation:** `ListInstallations` (the GET, not the background poller) does a synchronous `h.ghClient.ListInstallations(ctx)` if the store is empty (dashboard.go:65-87), then a synchronous `UpsertInstallations` if that succeeded. Both errors are silently swallowed (`if err == nil` is the only path that takes effect). The HTTP request gets a 200 with an empty list. The `h.logger.Warn` line for the GitHub call is reached only if the GitHub call fails; the `UpsertInstallations` failure is not logged at all.

**Impact:** A receiver that comes up after a long downtime (e.g. a maintenance window) shows an empty installations list for the duration of the 5-min poll if the cold-start refresh fails for any reason. No log line tells the operator the cold-start was attempted. Combined with the next poll (also silent on failure), the dashboard's "X repos installed Boop" KPI is wrong for up to 5 min with no signal.

**Suggestion:** Log the `UpsertInstallations` error at `Warn`:
```go
if err := h.store.UpsertInstallations(ctx, installs); err == nil {
    rows = installs
    fetchedAt = time.Now().UTC()
} else {
    h.logger.Warn("cold start refresh upsert", "err", err)
}
```

---

### EH-011
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/runner/src/lib/dashboard.mjs:135` — `startHeartbeat` `.catch(() => {})` |

**Observation:** The heartbeat tick wraps `postWithRetry` in a `.catch(() => {})`. `postWithRetry` already swallows its own errors and logs them at the `dashboard` channel; the extra `.catch` is defensive against a future refactor that might let an exception escape. The catch is correct but the `() => {}` is the kind of pattern that future maintainers extend to add a `console.log` or a `setTimeout` retry without considering the heartbeat's intent. A comment explaining why the swallow is OK would help.

**Impact:** None today (postWithRetry doesn't throw). The risk is a future refactor that adds side-effects inside the catch.

**Suggestion:** Comment the catch with the intent:
```js
postWithRetry(url, "", ctx.dashboardToken, deps).catch((err) => {
  // Should not happen — postWithRetry swallows its own
  // errors. Logged here as a backstop so a refactor that
  // breaks the contract surfaces in the heartbeat's log
  // rather than as an unhandled rejection.
  deps.log("dashboard", "heartbeat post threw", { err: String(err?.message ?? err) });
});
```

---

### EH-012
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                       |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/rerun.go:92` — `RerunPreview` ignores `GetTelemetry` error |

**Observation:** `telem, _ := h.store.GetTelemetry(r.Context(), id)` discards the error. A real store error (e.g. DB lock contention) is indistinguishable from "this run had no telemetry" (a parse-fail run). The preview response's `Model` field is empty in both cases. The operator sees "no model" and cannot tell whether the prior was a parse-fail (expected) or whether the store is sick (actionable).

**Impact:** Low — store errors are rare in this hot path. The operator's diagnostic experience is degraded when the store does fail.

**Suggestion:** Log and surface a sentinel in the response, e.g. `Model: telem.Model, ModelSource: "telemetry|missing"`. The dashboard can render "no model (no telemetry row)" vs. "no model" with the same visual but a different tooltip.

---

### EH-013
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                        |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/dashboard/dashboard.go:484-494` — `Health` returns 500 + plain-text body |

**Observation:** `Health` (the dashboard's liveness endpoint) returns 500 with body `"store error"` on a `Stats` failure. The receiver's `/health?deep=1` (handler.go:290-329) returns a JSON body with the `quick_check` result and stat counts. The dashboard's `Health` is less informative — operators curling the endpoint see a 500 with no detail. The kubelet only cares about the status code, so the functional impact is nil.

**Impact:** Cosmetic. The endpoint works for the probe; it just doesn't help an operator debug.

**Suggestion:** Mirror the receiver's deep-health shape:
```go
w.Header().Set("Content-Type", "application/json")
w.WriteHeader(http.StatusInternalServerError)
_, _ = w.Write([]byte(`{"status":"error","detail":"store unreachable"}`))
```

---

### EH-014
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                        |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/store/audit.go:187-193` — `MarshalDetails` silently returns "" on JSON failure |

**Observation:** `MarshalDetails` returns `""` when `json.Marshal` fails. A caller that passes a value with a non-serializable field (e.g. a `chan int`, a function) loses the details blob but still writes the audit row. The comment acknowledges this is intentional — the audit log should not lose an event because the details shape was malformed. But the caller's failure mode is silent: they get back an event with `Details == ""` and no error.

**Impact:** Rare. The caller pattern today uses plain `map[string]any` shapes that always serialize. The risk is a future caller that adds a complex type.

**Suggestion:** Change the return to `(string, error)` and let callers decide. Or, add a `MarshalDetailsStrict` that errors for callers that want to know. Today, `MarshalDetails` is the right shape (the comment is correct); the optional strict variant is a future addition.

---

### EH-015
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                        |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/dashboard/dashboard.go:474-478` — `renderJSON` is unused and untested |

**Observation:** `renderJSON` is documented as a "debug helper used during development" and is not wired to any route. The function is exported (lowercase, package-internal) and the docstring is clear that it is dead code today. It is fine to keep around for a future curl-friendly debug endpoint, but the comment is honest about it.

**Impact:** None.

**Suggestion:** Either wire it to a `/dashboard/debug` route gated by the dashboard token (useful for operators) or remove it. Leaving dead code is a footgun for the next refactor.

---

### EH-016 (Positive note)
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 N/A                                             |
| Decide   | Leave as-is                                        |
| Location | `apps/receiver/internal/store/stages.go:45-75` — `UpsertRunStage` ON CONFLICT clause |

**Observation:** The `UNIQUE(run_id, stage)` constraint plus the `ON CONFLICT(run_id, stage) DO UPDATE SET ended_at = COALESCE(excluded.ended_at, run_stages.ended_at), ...` clause is the load-bearing correctness rule for the at-least-once stage POST contract. A re-delivered end POST that lands after the row is already closed cannot accidentally re-open a closed stage. A re-delivered start POST cannot clobber a closed stage's `ended_at`. The COALESCE is the right shape: new value wins if set, otherwise keep existing. This is a clean example of an idempotency contract enforced in the SQL, not the application.

**Impact:** Defense-in-depth. Worth keeping.

---

## Unable to Verify

- **Whether the 4xx path on `postWithRetry` (dashboard.mjs:213-215) is ever hit with 401 in production today** — the operator can misconfigure `BOOP_DASHBOARD_TOKEN` vs. `BOOP_RUNNER_TOKEN`, but the receiver's `checkRunnerToken` (dashboard.go:445-454) returns 401 for any token mismatch, so a misconfigured runner would see 401 on every POST. The current test (`dashboard.test.mjs:96-101`) exercises a single 401 and confirms the no-retry behavior. The escalation in EH-007 is correct regardless of the production frequency.
- **Whether the runner's `postWithRetry` is ever called with `dashboardToken = ""` in production** — the runner's `index.mjs:181` only starts the heartbeat after `startHeartbeat` returns; the no-op short-circuit at `dashboard.mjs:124-126` catches the empty case. The other helpers (`postStatus`, `postTelemetry`, etc.) check the same. A race where the env var is cleared mid-run would surface as a fetch with an empty `X-BOOP-Runner-Token` header, which the receiver treats as 401.
- **Concurrent re-run frequency** — operators manually click "re-run". The TOCTOU in EH-008 needs two simultaneous clicks. The rate is bounded by the dashboard's "double-click → single submit" JS, which I did not audit. If the dashboard has a debounce, the race is impossible in practice.
- **The receiver's full middleware chain on `/api/runs/{id}/stages` etc.** — I confirmed the routes are registered with the default mux in `main.go` (per the diff). I did not verify the request body limit (`1<<16` per the handler) is appropriate for the lens_telemetry batch — a PR with hundreds of lenses would exceed it. Today, the runner emits one row per `lens: <name>` marker; the number of lenses is small.
- **Whether the `stopHeartbeat()` call in `index.mjs:353` runs even when the process is mid-fetch on the first tick** — the `finally` block awaits `stopHeartbeat()` synchronously. The first tick is 30s after start, so by the time it fires, the run is either done (in which case `stopped = true` and the tick is a no-op) or in progress (in which case the tick fires, the in-flight POST resolves, and the next tick is suppressed). The `unref` on the timer (dashboard.mjs:145) ensures the timer alone cannot keep the process alive past `run()`'s return. Functionally correct.

---

## Counts

| Tier          | Count |
|---------------|-------|
| 🔴 Blocking   | 3     |
| 🟡 Follow-up  | 7     |
| 🟢 Optional   | 3     |

Next: open `audits/error-handling.md` and skim EH-001, EH-002, EH-003 first — those are the silent-data-loss bugs that the dashboard can hide for hours. EH-006 (audit log not wired) is the contract gap that compliance will flag.
