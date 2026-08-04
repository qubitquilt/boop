# Error Handling Audit
**Date:** 2026-08-04
**Scope:** QUB-94 — `feature/qub-94-migrate-boop-runner-from-opencode-cli-to-openrouter-sdk`
**Files reviewed:**
- `apps/receiver/cmd/receiver/main.go`
- `apps/receiver/internal/webhook/handler.go`
- `apps/receiver/internal/webhook/handler_test.go`
- `apps/receiver/internal/webhook/jobbuilder.go`
- `apps/runner/src/lib/config.mjs`
- `apps/runner/src/lib/opencode.mjs` (added `runOpenRouterSkill`)
- `apps/runner/src/lib/openrouter.mjs` (new file)

---

## Summary

The new SDK code path has solid structural resilience: the timeout (`OPENCODE_TIMEOUT_MS = 25 * 60 * 1000` from `config.mjs:39`) is wrapped in an `AbortController` with a single firing point, the catch block in `runOpenRouterSkill` distinguishes `AbortError` from other failures with a clean throw-vs-return split, and `buildTelemetry(null, err)` safely stamps `error` on the empty-telemetry row without dereferencing `err.message` blindly. **Highest-risk gap is EH-003:** the user-facing PR status comment collapses every SDK failure category into the categorical string `"sdk call failed"`, losing the HTTP status code (4xx vs 5xx vs network) that an operator needs to triage a bad run when the dashboard data layer is off.

---

## Findings

### EH-001
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟡 Follow-up                                                          |
| Decide   | Change now                                                            |
| Location | `apps/receiver/internal/webhook/handler.go:565` — `submitJob`         |

**Observation:** The `sdk flag resolved` log line is emitted at `slog.LevelDebug`. The receiver's default `LOG_LEVEL` is `Info` (`main.go:138` falls through to `LevelInfo` when `LOG_LEVEL` is empty or unrecognized), so during normal operation an operator investigating a misconfigured PR will not see why the Job landed on either path. The line carries the exact fields needed to debug (`value`, `label_present`, `cluster_default`, `delivery`).

**Impact:** When a PR lands on the SDK path unexpectedly — or fails to land on the SDK path after the operator added `boop:openrouter-sdk` — the operator must restart the receiver with `LOG_LEVEL=debug` and re-trigger a webhook to see the decision. The flag is per-PR; this is a regression hazard during rollout.

**Suggestion:**
```go
h.logger.Info("sdk flag resolved", "delivery", delivery, "value", sdkEnabled,
  "label_present", hasLabel(labels, sdkEnabledLabel),
  "cluster_default", h.cfg.OpenRouterSDKDefault)
```
The line is emitted once per PR review (low volume) and carries no secret material.

---

### EH-002
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟢 Optional                                                           |
| Decide   | Defer                                                                 |
| Location | `apps/runner/src/lib/openrouter.mjs:285-295` — `readOpencodeModel`    |

**Observation:** The catch block returns `""` silently for any read or parse failure of the opencode.json mount, including the transient `..data -> ..2026_…` symlink race that the rest of the codebase already handles with `readWithRetry` in `opencode.mjs:75-89`. The caller (`runOpenRouterSkill:728-735`) then throws `"openrouter SDK path: no model configured"` with no breadcrumb about WHY the model resolution failed.

**Impact:** Operator triaging a failed run sees "no model configured" but cannot distinguish: (a) the ConfigMap mount is missing, (b) the file is malformed JSON, (c) the mount's symlink race persisted past the existing 5-attempt budget, or (d) the env override `OPENROUTER_MODEL` is unset. The dashboard row has the same opaque message via the error pipeline.

**Suggestion:** Thread `deps.errlog` into `readOpencodeModel` and log a breadcrumb before returning `""`. Mirror the `readWithRetry` onRetry pattern (`opencode.mjs:82-84`) so transient symlink failures get the same treatment.

---

### EH-003
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟡 Follow-up                                                          |
| Decide   | Defer                                                                 |
| Location | `apps/runner/src/lib/opencode.mjs:788-793` — `runOpenRouterSkill`     |

**Observation:** When the SDK call fails for any non-abort reason (4xx, 5xx, network, malformed response), `runOpenRouterSkill` stamps `review.parseError = "sdk call failed"` and routes the real `err.message` only into `telemetry.error` via `buildTelemetry`. The user-facing PR status comment becomes `"summary parse failed: sdk call failed"` (the `summaryGate` at `workflow.mjs:453` joins the gate reason with the parseError). The underlying HTTP status code, the model name, the SDK error message — none of these surface to the PR author.

**Impact:** When the dashboard data layer is enabled (default for `RUNNER_TOKEN` set), the operator can still triage via the `telemetry.error` field. When the data layer is disabled (`RUNNER_TOKEN` unset, the receiver logs this as `Warn` at `main.go:80-82`), the PR author sees only the categorical string and has no signal beyond "check the Job logs." The trade-off was intentional (parseError is a categorical reason across all parse-failure modes) but loses actionable detail on the path most likely to fail during rollout.

**Suggestion:** Two reasonable fixes — pick one before the cutover PR:

```js
// Option A: enrich parseError with the HTTP status when available.
const status = err?.cause?.statusCode ?? err?.statusCode;
review.parseError = status
  ? `sdk call failed (HTTP ${status})`
  : "sdk call failed";
```

```js
// Option B: keep parseError categorical but stamp err.message on a
// new review field that the post pipeline surfaces.
return {
  ...review,
  parseError: "sdk call failed",
  parseErrorDetail: String(err?.message ?? err),
  telemetry: buildTelemetry(null, err),
};
```

Either change should be paired with a workflow-level test that asserts the status comment carries the underlying detail.

---

### EH-004
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟢 Optional                                                           |
| Decide   | Leave as-is                                                           |
| Location | `apps/runner/src/lib/opencode.test.mjs:702-722` — `makeSdkDeps`       |

**Observation:** The test stub for `deps.callOpenRouter` returns `{...callResult, ...opts, _args: opts}` where `opts.env = { OPENROUTER_API_KEY: openrouterApiKey }`. Production `callOpenRouter` (`openrouter.mjs:118-122`) returns only `{ text, usage, model }` and never the env. A future refactor that copies the test's "spread `opts` into the result" pattern into production code would silently leak the API key into the dashboard POST (the telemetry object is JSON-serialized into the receiver's `POST /api/runs/{id}/telemetry` body).

**Impact:** Test-only today, so no production data leak. The risk is a regression where someone reads the test, sees the spread, and replicates it. Worth tightening the test so it doesn't normalize the pattern.

**Suggestion:**
```js
return {
  text: callResult.text,
  usage: callResult.usage,
  model: callResult.model,
  _args: { model: opts.model, hasEnv: !!opts.env }, // redact the env
};
```

---

### EH-005
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟡 Follow-up                                                          |
| Decide   | Defer                                                                 |
| Location | `apps/receiver/internal/webhook/handler.go:827-835` — `resolveSDKEnabled` |

**Observation:** `resolveSDKEnabled` treats any value other than `"1"` as the floor (returns `"0"`), but `getenv` in `main.go:124-129` forwards any non-empty string. An operator who sets `BOOP_USE_OPENROUTER_SDK=true` (a common Helm/Kubernetes convention) sees `cluster_default=true` in the debug log but `value=0` in the Job env. There is no validation that the env var is one of `"0"`, `"1"`, or empty.

**Impact:** Misconfiguration that silently disables the SDK path. Operators checking `kubectl logs` at default level (per EH-001) won't see the mismatch. The behavior is technically correct (only `"1"` is truthy) but the failure mode is invisible.

**Suggestion:** Validate at receiver startup, log at `Warn` level:
```go
if cfg.OpenRouterSDKDefault != "" && cfg.OpenRouterSDKDefault != "0" && cfg.OpenRouterSDKDefault != "1" {
  logger.Warn("BOOP_USE_OPENROUTER_SDK has unexpected value; expected 0, 1, or empty",
    "value", cfg.OpenRouterSDKDefault)
}
```

---

### EH-006
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟢 Optional                                                           |
| Decide   | Leave as-is                                                           |
| Location | `apps/runner/src/lib/opencode.mjs:772` — `runOpenRouterSkill`         |

**Observation:** The AbortError detection is a string match: `err?.name === "AbortError"`. The unit test (`openrouter.test.mjs:181-185`) constructs an error with `name = "AbortError"` manually, so the runner's behavior is pinned. The real `@openrouter/sdk` abort path is unverified — if the SDK surfaces the abort as a wrapped error, a `DOMException` with a different `name`, or with `code === "ABORT_ERR"`, the catch block treats it as a regular SDK failure and returns a parseError review (e.g., `parseError = "sdk call failed"`) instead of throwing the timeout.

**Impact:** A miscategorization turns a 25-minute timeout into a parseError review, which would still avoid posting to the PR (good) but would surface a misleading "sdk call failed" instead of "openrouter run exceeded 25-min timeout" on the PR status comment. The dashboard row would also show the wrong category.

**Suggestion:** Broaden the check defensively, then add an integration test that uses the real SDK and pins the abort surface:
```js
const isAbort = err?.name === "AbortError" ||
  err?.code === "ABORT_ERR" ||
  err?.name === "TimeoutError";
```

---

### EH-007
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟢 Optional                                                           |
| Decide   | Leave as-is                                                           |
| Location | `apps/runner/src/lib/opencode.mjs:732-736` — `runOpenRouterSkill`     |

**Observation:** The "no model configured" throw path is exercised by neither `opencode.test.mjs` nor `openrouter.test.mjs`. A future regression that breaks `readOpencodeModel`, the `ctx.openrouterModel` env wiring in `config.mjs:135`, or the env propagation in `jobbuilder.go:307` would not be caught by tests.

**Impact:** Silent regression — production PRs would all fail with the same throw until someone debugs the Job logs. The throw message itself is actionable (mentions both `OPENROUTER_MODEL` and `opencode.json`), so diagnosis is possible, but a unit test would be cheap.

**Suggestion:**
```js
test("runOpenCodeSkill throws when neither OPENROUTER_MODEL nor opencode.json is available", async () => {
  const deps = makeSdkDeps(...); // no model in callResult
  deps.paths.configSrc = "/nonexistent";
  await assert.rejects(
    () => runOpenCodeSkill("api-key", { ...SDK_BASE_CTX, openrouterModel: null }, deps),
    /no model configured/,
  );
});
```

---

### EH-008
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟢 Optional                                                           |
| Decide   | Leave as-is                                                           |
| Location | `apps/runner/src/lib/opencode.mjs:764` — `runOpenRouterSkill`         |

**Observation:** The SDK call forwards `env: { OPENROUTER_API_KEY: openrouterApiKey }` — only this single key, no `PATH`, `HOME`, `NODE_ENV`, or proxy settings. The subprocess path (`runOpencode` at `opencode.mjs:518-533`) takes the opposite approach with a near-empty allowlist and explicit scrubbing. If a future SDK version reads proxy env vars or debug flags, they'd be silently missing.

**Impact:** Low risk today (the SDK only consumes `OPENROUTER_API_KEY` per the comment in `callOpenRouter` at `openrouter.mjs:9-13`). The asymmetry with the subprocess path's scrubbing is a footgun for future maintenance.

**Suggestion:** Document the asymmetry in `callOpenRouter`'s JSDoc and add an inline comment at `opencode.mjs:764` explaining why the SDK path is allowed to forward the key but is intentionally narrower than the subprocess env.

---

### EH-009 (Positive note)
| Field    | Value                                                                 |
|----------|-----------------------------------------------------------------------|
| Tier     | 🟢 N/A                                                                |
| Decide   | Leave as-is                                                           |
| Location | `apps/runner/src/lib/openrouter.test.mjs:201-229`                     |

**Observation:** The "does not read files or spawn processes" test pins the SDK module's surface area as purely network. If a future change accidentally introduces a `fs.readFile` or `child_process.spawn` call into the SDK path, this test fails loudly. This is the kind of regression test that pays for itself the first time someone tries to "just" read a config file from the SDK.

**Impact:** Defense-in-depth. Worth keeping.

---

## Unable to Verify

- **SDK's actual abort surface shape** — `openrouter.test.mjs` only exercises a hand-rolled `AbortError`. The real `@openrouter/sdk@1.2.8` may wrap the abort in a way that bypasses `err.name === "AbortError"`. To confirm, run the runner with `BOOP_USE_OPENROUTER_SDK=1` against a hung endpoint and inspect the `errlog` line for `errorName`.
- **Whether the SDK includes the request URL or API key in `err.message`** for non-abort failures — `callOpenRouter` at `openrouter.mjs:96-110` extracts `err.message` and logs it via `errlog` + stamps it into `telemetry.error`. If the SDK includes the URL (which might contain the key as a query param), it would surface in dashboard rows. OpenRouter uses Authorization headers, so this is low probability, but unverified.
- **End-to-end dashboard rendering of `telemetry.error`** — the field is set on the empty-telemetry object at `openrouter.mjs:231`, but whether the dashboard UI distinguishes failed-call rows from successful-empty-summary rows depends on the dashboard's render layer (not in scope of this PR's changed files).

---

## Counts

| Tier          | Count |
|---------------|-------|
| 🔴 Blocking   | 0     |
| 🟡 Follow-up  | 3     |
| 🟢 Optional   | 5     |

Next: open `audits/error-handling.md` and skim EH-001 + EH-003 first — those are the highest-leverage changes before the cutover.
