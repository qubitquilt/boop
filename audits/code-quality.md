# Code Quality Audit
**Date:** 2026-08-04
**Scope:** QUB-94 PR `feature/qub-94-migrate-boop-runner-from-opencode-cli-to-openrouter-sdk`
**Files reviewed (changed code only):**

- `apps/receiver/cmd/receiver/main.go` (+7)
- `apps/receiver/internal/webhook/handler.go` (+56/-3)
- `apps/receiver/internal/webhook/handler_test.go` (+59)
- `apps/receiver/internal/webhook/jobbuilder.go` (+16/-7)
- `apps/runner/package.json`, `apps/runner/package-lock.json` (new dep `@openrouter/sdk@1.2.8`)
- `apps/runner/src/lib/config.mjs` (+13)
- `apps/runner/src/lib/opencode.mjs` (+138/-4)
- `apps/runner/src/lib/opencode.test.mjs` (+83)
- `apps/runner/src/lib/openrouter.mjs` (+296, NEW FILE)
- `apps/runner/src/lib/openrouter.test.mjs` (+306, NEW FILE)
- `docs/development.md` (+94)

---

## Summary

The SDK path is well-scoped and the new `openrouter.mjs` is a textbook seam: pure I/O at the boundary, telemetry mapped in one place, a single dependency-injection knob (`deps.client`) for tests. The complexity lives in the orchestrator — `runOpenRouterSkill` in `opencode.mjs` mixes model resolution, SDK plumbing, error classification, telemetry stamping, and review-shape construction in one 105-line function. The error path is dense enough that the next change to "what an SDK failure looks like" will need to touch three concerns in one block. Test coverage is solid on the happy path, the failure path, the abort path, and the legacy fallback, but the new `readOpencodeModel` helper is exported without a direct test and the orchestrator's "no model configured" throw is uncovered. Receiver-side additions are clean: small, focused, well-tested.

**Highest-risk area:** `runOpenRouterSkill` in `opencode.mjs:716` — the catch block at `:766-794` interleaves error logging, telemetry construction, and review-shape mutation in a way that will obscure the next failure-mode change.

---

## Findings

### CQ-001
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/runner/src/lib/opencode.mjs:716` — `runOpenRouterSkill()` (105 lines) |

**Observation:** `runOpenRouterSkill` performs five distinct jobs in one linear flow: (1) prompt construction with a `skipSkill` ternary, (2) three-step model resolution (`ctx.openrouterModel` → `readOpencodeModel` → throw), (3) status / start-log, (4) the SDK call wrapped in a 30-line try/catch that classifies `AbortError` vs. generic failure, logs to errlog, and on generic failure calls `parseReviewOutput("")` then mutates `parseError` then returns `{ ...review, telemetry: buildTelemetry(null, err) }`, (5) parse output + exit log + parse-error log + final return. The catch block alone is the densest patch of new code in the PR.

**Impact:** Cyclomatic complexity is 7-8 (under 10) so the function is not flag-worthy by that metric alone, but the cognitive load is real. The next change to "what an SDK failure looks like" will land in a 30-line block that holds error classification + breadcrumb logging + review-shape construction + telemetry stamping. The `parseReviewOutput("")` + `parseError` mutation is the most surprising pattern — it relies on the parser's failure shape being a mutable plain object, which is an undocumented coupling to `opencode.mjs:169` and would break if `parseReviewOutput` ever returns a frozen object or a class instance. None of this is blocking, but the function will become the second-largest in `opencode.mjs` and the next reader has to keep five concerns in their head.

**Suggestion:** Split the orchestrator. The "telemetry on failure" return shape and the `parseReviewOutput("")` trick are easy to extract:

```javascript
// In opencode.mjs — a small helper next to parseReviewOutput
function emptyReview(parseError) {
  const r = parseReviewOutput("");
  r.parseError = r.parseError || parseError;
  return r;
}

// Then runOpenRouterSkill shrinks to:
async function runOpenRouterSkill(openrouterApiKey, ctx, deps) {
  const prompt = ctx.skipSkill
    ? `Reply with one sentence: confirm you can see the repo at ${deps.paths.repoDir} on head ${shortSha(ctx.prHeadSha)}.`
    : await buildBoopPrompt(ctx, deps);
  const model = await resolveSdkModel(ctx, deps);     // throws if absent
  await deps.postStatus("review");
  deps.log("opencode", "starting", { dir: deps.paths.repoDir, model, mode: ctx.skipSkill ? "minimal" : "full", path: "openrouter-sdk" });
  const result = await invokeSdkCall(openrouterApiKey, prompt, model, deps); // throws on timeout, returns review+telemetry otherwise
  // ...parse + return unchanged
}
```

The PR's own comment at `opencode.mjs:712-715` already names the cross-module surface as "one call to `callOpenRouter` and one call to `buildTelemetry`" — the function's body doesn't yet match that stated surface.

---

### CQ-002
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/runner/src/lib/opencode.mjs:716` — `runOpenRouterSkill()`; `apps/runner/src/lib/openrouter.mjs` is a NEW FILE |

**Observation:** `opencode.mjs` already hosts the `opencode` CLI pipeline (`materializeConfig`, `runOpencode`, `runOpencodeJSON` glue), the boop prompt builder, and the parser. After this PR it also hosts `runOpenRouterSkill` — a function whose body touches none of the opencode subprocess machinery and instead reads from the opencode.json ConfigMap only as a fallback model source. The file-level docstring at `opencode.mjs:1-8` describes "the opencode CLI pipeline" and reads as a historical artifact. The PR's own comment at `opencode.mjs:712-715` acknowledges the awkwardness: "The function lives in opencode.mjs (not openrouter.mjs) so it can reuse `buildBoopPrompt` + `parseReviewOutput` without a circular import."

**Impact:** The circular-import justification is real, but it leaves `opencode.mjs` with two unrelated pipelines under one name. After QUB-98 deletes the opencode subprocess, the file will contain only the SDK path, the prompt builder, and the parser — at which point "opencode.mjs" is a wrong name and the rename becomes a bigger refactor. The longer the dual-purpose file sits, the more code (and tests) anchor to the misleading name. Note that the function placement also means `runOpencodeJSON` (`opencode_json.mjs`) is still imported from `opencode.mjs` via `importRunOpencodeJSON` — a third "pipeline" lives in the same file by lazy import.

**Suggestion:** Defer the rename to QUB-98 when the opencode branch is removed. Add a TODO comment at the top of `opencode.mjs` flagging the rename target (`boop_review.mjs` is the candidate) so the next reader doesn't have to rediscover this. Not blocking because the dual-purpose state is acknowledged in the PR's own comments and is on the cutover path.

---

### CQ-003
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/runner/src/lib/opencode.mjs:766-794` — error path inside `runOpenRouterSkill()` |

**Observation:** The `catch (err)` block at `opencode.mjs:766-794` has three separate concerns interleaved: (a) `isAbort` classification and the `killed`/`timeoutMs` flag, (b) errlog breadcrumb with `errorName` and `elapsedMs`, (c) a review-shape construction that calls `parseReviewOutput("")`, mutates `parseError` if absent, and wraps with `buildTelemetry(null, err)`. The mutation of `review.parseError` is the most surprising line: `parseReviewOutput("")` already returns `parseError: "no structured block"` (see `opencode.mjs:178`), so `review.parseError = review.parseError || "sdk call failed"` only changes the string when the parser's own error is absent — but `parseReviewOutput("")` always sets it, so the `||` branch is dead in the SDK-failure case. The intent (override "no structured block" with "sdk call failed") is correct but the code expresses "preserve any existing parseError" instead.

**Impact:** This is the place a future debugging pass will land when an SDK call starts failing in a new way. The interleaving makes it hard to see the order of operations, and the dead `||` branch hides a behavior the next reader has to re-derive. The `elapsed` variable is computed once but only used in the errlog; the `killed` / `timeoutMs` pair is set inside the `if (isAbort)` and consumed only by the immediate throw, so it could be inlined. None of this is a bug, but the next change here will be in a block where each line does two things.

**Suggestion:** Defer the split (it overlaps with CQ-001). At minimum, fix the dead-branch surprise so the intent is obvious: `review.parseError = "sdk call failed"` — but that drops the parser's "no structured block" signal, which is the wrong tradeoff. The honest fix is to either accept the parser's error string verbatim (delete the `|| "sdk call failed"`) or to assert it: `if (!review.parseError) review.parseError = "sdk call failed";`. The latter is what the code intends but only the former is what it does. Change the line to make the intent literal and the dead branch is gone.

---

### CQ-004
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now |
| Location | `apps/runner/src/lib/openrouter.mjs:285` — `readOpencodeModel()` (exported, not directly tested) |

**Observation:** `readOpencodeModel` is exported from `openrouter.mjs` and called from `runOpenRouterSkill` at `opencode.mjs:730`, but it has no direct unit test in `openrouter.test.mjs` (the import block at line 4-8 lists only `buildTelemetry`, `callOpenRouter`, `emptyTelemetry`). The function swallows `JSON.parse` errors and `fs.readFile` errors and returns `""`, so the contract has at least four code paths (missing `configSrc`, read failure, parse failure, `model` field missing or non-string). It is exercised today only by the orchestrator's happy-path test which sets `openrouterModel` directly and never enters the `readOpencodeModel` branch.

**Impact:** The next time the opencode.json shape changes (e.g., a `models` array is introduced, or the file moves to a different mount path), the failure surface of this function will be tested for the first time in production. Because the function is the fallback for the post-QUB-98 cutover (the PR comment at `openrouter.mjs:267-280` calls this out), it is on the upgrade path — a missing test now means the cutover is the test.

**Suggestion:** Add three direct tests in `openrouter.test.mjs`:

```javascript
test("readOpencodeModel returns the model field when present", async () => {
  const deps = { fs: { readFile: async () => '{"model":"minimax/minimax-m3"}' }, paths: { configSrc: "/etc/oc" } };
  assert.equal(await readOpencodeModel(deps), "minimax/minimax-m3");
});
test("readOpencodeModel returns \"\" when the file is missing", async () => {
  const deps = { fs: { readFile: async () => { throw new Error("ENOENT"); } }, paths: { configSrc: "/etc/oc" } };
  assert.equal(await readOpencodeModel(deps), "");
});
test("readOpencodeModel returns \"\" when the model field is missing", async () => {
  const deps = { fs: { readFile: async () => '{"provider":{}}' }, paths: { configSrc: "/etc/oc" } };
  assert.equal(await readOpencodeModel(deps), "");
});
```

---

### CQ-005
| Field    | Value |
|----------|-------|
| Tier     | 🟡 Follow-up |
| Decide   | Change now |
| Location | `apps/runner/src/lib/opencode.mjs:732-736` — `if (!model) { throw new Error(...) }` |

**Observation:** The orchestrator's "no model configured" throw is uncovered by `opencode.test.mjs`. The five new SDK-branch tests all set `openrouterModel: "minimax/minimax-m3"` on the base ctx (`opencode.test.mjs:734`) and never enter the resolution path that calls `readOpencodeModel` or triggers the throw. The error message is the operator's only signal that the cluster is missing both `OPENROUTER_MODEL` and the opencode.json ConfigMap — a misconfiguration during the QUB-98 cutover.

**Impact:** The cutover (QUB-98) deletes the opencode.json ConfigMap. From that point on, this throw is the gate between a successful SDK run and a "no model configured" failure. The first time the throw fires in production will be the first time it is tested.

**Suggestion:** Add one orchestrator test in `opencode.test.mjs`:

```javascript
test("runOpenCodeSkill throws when no model is configured on the SDK path", async () => {
  const deps = makeSdkDeps({ text: "x", usage: { prompt_tokens: 1, completion_tokens: 1, cost: 0 }, model: "x" });
  // openrouterModel is unset on ctx; readOpencodeModel returns "" by design.
  // The orchestrator should throw before the SDK is called.
  await assert.rejects(
    () => runOpenCodeSkill("api-key", { ...SDK_BASE_CTX, openrouterModel: null }, deps),
    /no model configured/,
  );
});
```

The test doubles as documentation of the cutover's failure mode.

---

### CQ-006
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Defer |
| Location | `apps/runner/src/lib/opencode.mjs:811-817` — `if (review.parseError) { deps.log("review", "summary_parse_failed", ...) }` |

**Observation:** The "summary_parse_failed" log path on the SDK branch is not tested in `opencode.test.mjs`. The legacy subprocess path covers this case (a `parseError`-bearing review from `parseReviewOutput` triggers the log) and the SDK-branch test at `opencode.test.mjs:739` always passes a well-formed `SDK_REVIEW_BODY`, so the log line is unexercised.

**Impact:** Low — the log path is identical to the legacy path's behavior, and the parser already has dedicated tests. The only SDK-specific concern is that `callResult.text` (the SDK response) might be longer than the legacy stdout in unusual ways, but the log slices the first 200 bytes either way. Not blocking; flag for completeness.

**Suggestion:** Add a parse-failure test in the next change that touches the SDK branch. Skip for this PR.

---

### CQ-007
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/opencode.mjs:752` — `const callFn = deps.callOpenRouter || callOpenRouter;` |

**Observation:** `runOpenRouterSkill` accepts a `deps.callOpenRouter` override but `runOpenCodeSkill` itself does not — the SDK function comes from the module-level import. This is the opposite of the rest of the runner's pattern (`deps.runOpencodeJSON` with `importRunOpencodeJSON` fallback, `deps.spawnFn` for `runOpencode`, `deps.setTimeoutFn` for the timer). The seam is clear and the comment at `opencode.mjs:750-751` is honest about it ("Test injection point: `deps.callOpenRouter` overrides the real SDK call. Production code calls the SDK directly."), but the pattern inconsistency is a small smell.

**Impact:** The seam works — tests inject `deps.callOpenRouter` and the function honors it. But a future reader will wonder why one orchestrator uses `deps.fn || moduleFn` and the other uses `moduleFn` (with the test plumbing an in-process override a different way). This is a stylistic call; either pattern is fine if it's consistent. Not blocking.

**Suggestion:** Leave as-is. The `deps.callOpenRouter` override is genuinely needed (the tests use it) and the fallback is documented. If CQ-001's split happens, consider routing both through `deps` for uniformity — but it's a stylistic refactor, not a quality issue.

---

### CQ-008
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/openrouter.mjs:137-150` — `extractAssistantText` |

**Observation:** `extractAssistantText` handles two content shapes (string vs. array of `ChatContentItems`) and falls back to `""` for everything else. The array path joins parts with `out.join("")` and assumes every part with a `.text` field is a string — it does not distinguish `type: "text"` parts from other types (e.g., `type: "image_url"` would be silently dropped if it lacked a `.text` field, and parts that are neither string nor object would also be silently dropped).

**Impact:** Forward-compatibility is good for the common case. The boop review is always a single text block, so the array path is for SDK evolution only. If a future model returns a `tool_call` part, it is dropped silently — which is the right behavior for this runner, but the test at `openrouter.test.mjs:100-120` only covers the `type: "text"` array case. Not flag-worthy as a bug, just a note that the array path's behavior on non-text parts is implicit.

**Suggestion:** Leave as-is. The forward-compat test covers the common shape. If tool-call support is added later, the array path needs explicit handling.

---

### CQ-009
| Field    | Value |
|----------|-------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/receiver/internal/webhook/handler.go:827-835` — `resolveSDKEnabled` |

**Observation:** `resolveSDKEnabled` is 9 lines, 2 branches, and well-tested with a 9-case table at `handler_test.go:179-207`. The function is small enough that any further refactor (e.g., a lookup table) would be cosmetic. The case-insensitive `hasLabel` lookup is a deliberate choice covered by the `Boop:OpenRouter-SDK` test row.

**Impact:** None — this is the cleanest new code in the PR. The test table even exercises the cluster-default-unset case (`""`) which is easy to miss.

**Suggestion:** Leave as-is. Positive note: the test at `handler_test.go:209-234` (`TestBuildJobForwardsOpenRouterSDKEnabled`) confirms the env var is actually injected into the Job spec — that's the kind of test that often gets forgotten in feature-flag PRs.

---

## Metrics at a Glance

| File | Function | CC | LOC | Notable coupling |
|------|----------|----|-----|------------------|
| `apps/runner/src/lib/opencode.mjs` | `runOpenRouterSkill` (NEW) | 7-8 | 105 | imports `callOpenRouter`/`buildTelemetry`/`readOpencodeModel` from `openrouter.mjs`; accepts `deps.callOpenRouter` override; reuses local `buildBoopPrompt` + `parseReviewOutput` to avoid circular import |
| `apps/runner/src/lib/opencode.mjs` | `runOpenCodeSkill` (CHANGED) | unchanged from baseline (~9) | unchanged | adds one early-return branch at top for SDK fast-path |
| `apps/runner/src/lib/openrouter.mjs` | `callOpenRouter` (NEW) | 4 | 84 | `deps.client` injection, `deps.AbortControllerCtor`, `deps.timeoutMs`; no module-level state |
| `apps/runner/src/lib/openrouter.mjs` | `buildTelemetry` (NEW) | 3 | 25 | pure mapping; handles `error` stamp on failure |
| `apps/runner/src/lib/openrouter.mjs` | `readOpencodeModel` (NEW) | 2 | 12 | `fs.readFile` + `JSON.parse`; silent failure → `""` |
| `apps/runner/src/lib/openrouter.mjs` | `extractAssistantText` (NEW) | 3 | 13 | pure; no deps |
| `apps/runner/src/lib/openrouter.mjs` | `extractUsage` (NEW) | 4 | 28 | pure; no deps |
| `apps/receiver/internal/webhook/handler.go` | `resolveSDKEnabled` (NEW) | 3 | 9 | pure given `h.cfg.OpenRouterSDKDefault` + `labels` |
| `apps/receiver/internal/webhook/handler.go` | `submitJob` (CHANGED) | unchanged | +9 | adds one new arg (`labels []string`) and one helper call; signature change rippled to `handlePullRequest` / `handleIssueComment` callers (the issue_comment path passes `nil` for labels) |
| `apps/runner/src/lib/opencode.test.mjs` | SDK-branch tests (NEW) | n/a | 83 (5 tests) | `deps.callOpenRouter` injection; `SDK_REVIEW_BODY` fixture shared across cases |

---

## Unable to Verify

- **Pre-existing line `// temporary verify mark 1785624554` at `opencode.mjs:821`.** This line is in the file at HEAD but not in the diff (`git diff HEAD~1 HEAD` does not show it being added), so it is pre-existing. Out of scope per the task constraints, but worth flagging to the author as a leftover editorial mark that should be removed.
- **Net `opencode.mjs` file growth to 821 lines.** The file was already a kitchen sink (parser, prompt builder, two orchestrators, ANSI stripper, shell quoter). The new function adds 105 lines to an 800-line file. Whether the file warrants splitting into `review/` and `orchestration/` submodules is a judgment call outside this PR's scope.
- **Test coverage of the env-shaped injection at `opencode.mjs:764`** (`env: { OPENROUTER_API_KEY: openrouterApiKey }`). The production code path looks correct (matches the contract `callOpenRouter` reads from `deps.env.OPENROUTER_API_KEY`), but no test in `opencode.test.mjs` asserts that the key actually reaches `callOpenRouter`. The `makeFakeClient` / `makeSdkDeps` fixture in `openrouter.test.mjs:15` is used by the SDK tests but `opencode.test.mjs` rolls its own simpler mock that captures the call args (`callOpenRouter: async (_prompt, opts) => { ...; return { ..., _args: opts }; }` at `opencode.test.mjs:710-719`) but the existing tests do not assert on `_args` — so the API-key plumbing is not pinned. A one-line `assert.equal(opts.env.OPENROUTER_API_KEY, "api-key")` would close this gap.
