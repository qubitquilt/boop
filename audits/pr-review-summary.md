# PR Review: feat(runner): migrate boop review to openrouter sdk (QUB-94)

**Date:** 2026-08-04
**Branch:** `feature/qub-94-migrate-boop-runner-from-opencode-cli-to-openrouter-sdk`
**Commit:** `a0f8090`
**Files reviewed:** 12 files, **+1150 / −14** (Go receiver + Node runner + docs)

---

## Executive Summary

The PR is a well-shaped, feature-flagged migration: the Go receiver resolves a cluster default + a per-PR label into a single `BOOP_USE_OPENROUTER_SDK` env var; the runner branches on a boolean and dispatches to a new in-process OpenRouter SDK call. The SDK boundary is clean (`@openrouter/sdk` is imported in exactly one new file, `openrouter.mjs`), the test seam mirrors the rest of the runner's pattern (`deps.callOpenRouter` override), and the QUB-98 deletion is mostly a one-file edit. **No blocking findings.** The two areas worth the author's attention are (1) the new `runOpenRouterSkill` orchestrator at `opencode.mjs:716` packs five jobs into 105 lines and the error path's `parseError = "sdk call failed"` collapses every non-abort failure mode (4xx/5xx/network) into a single string that the PR author sees when the dashboard data layer is off, and (2) three small documentation drifts — a misleading "cross-module surface" comment, a duplicate QUB-94 block in `runOpenCodeSkill`, and a debug-level log line that hides the SDK flag decision at default log level. Both are cheap to fix and one of them blocks operator triage on the rollout path.

**Merge-readiness signal: ready with minor changes.** Land the five change-now items below before merging; the rest can follow in QUB-98 or as a cleanup PR.

---

## Priority Issue Table

| ID              | Tier        | File : Line                                         | Summary                                                                                          | Decide       |
|-----------------|-------------|-----------------------------------------------------|--------------------------------------------------------------------------------------------------|--------------|
| EH-001          | 🟡 Follow-up | `apps/receiver/internal/webhook/handler.go:565`     | `sdk flag resolved` logged at Debug; invisible at default Info level during rollout              | Change now   |
| RD-007          | 🟡 Follow-up | `apps/runner/src/lib/opencode.mjs:585-600`          | Two QUB-94 comment blocks back-to-back; trim the in-body one                                      | Change now   |
| CQ-004 / RD-004 / EH-002 | 🟡 Follow-up | `apps/runner/src/lib/openrouter.mjs:285`     | `readOpencodeModel` exported but not directly tested; on the QUB-98 cutover path                  | Change now   |
| CQ-005 / EH-007 | 🟡 Follow-up | `apps/runner/src/lib/opencode.mjs:732`              | "no model configured" throw is uncovered; QUB-98 deletes the ConfigMap that hides the gap         | Change now   |
| DP-002 / RD-003 / SP-006 | 🟡 Follow-up | `apps/runner/src/lib/opencode.mjs:712-715`    | Comment claims the cross-module surface is "one call to `callOpenRouter` and one call to `buildTelemetry`" — actually three imports, wrong constraint | Change now   |
| EH-003          | 🟡 Follow-up | `apps/runner/src/lib/opencode.mjs:788-793`          | All non-abort SDK failures collapse to `parseError: "sdk call failed"`; loses HTTP status on the PR comment when dashboard is off | Defer        |
| EH-005          | 🟡 Follow-up | `apps/receiver/internal/webhook/handler.go:827-835` | `BOOP_USE_OPENROUTER_SDK=true` (Helm convention) silently treated as `"0"`                        | Defer        |
| RD-001 / SP-008 | 🟡 Follow-up | `apps/receiver/internal/webhook/handler.go:557`     | `submitJob` is now 15 positional args; test file needs `// reactionCommentID` inline labels        | Defer        |
| RD-002          | 🟡 Follow-up | `apps/receiver/cmd/receiver/main.go:57` + 4 other sites | `"0"` / `"1"` string flag repeats at 5 sites; no shared symbol                                   | Defer        |
| CQ-001 / SP-001 | 🟡 Follow-up | `apps/runner/src/lib/opencode.mjs:716`              | `runOpenRouterSkill` does 5 jobs in 105 lines; error-translation branch will be the next hot spot  | Defer        |
| CQ-003 / SP-005 | 🟡 Follow-up | `apps/runner/src/lib/opencode.mjs:766-794`          | Dense `catch` block + `parseReviewOutput("") || "sdk call failed"` is a dead-branch surprise        | Defer        |
| DP-003          | 🟡 Follow-up | `docs/development.md` (PR body)                    | QUB-98 framed as a "delete"; actually needs a rename + relocate + dep drop                         | Defer        |
| RD-010 / DP-005 | 🟢 Optional  | `apps/runner/src/lib/opencode.mjs:720-736`          | Model-resolution block comment names the source order; no log of which source won                | Defer        |
| CQ-002          | 🟢 Optional  | `apps/runner/src/lib/opencode.mjs`                  | File name no longer matches its content post-cutover; rename target not in this PR               | Defer        |
| EH-004 / RD-006 | 🟢 Optional  | `apps/runner/src/lib/opencode.test.mjs:702-722`     | Test mock's `...opts` spread + `_args` field are dead and could normalize a leaky pattern         | Leave as-is  |
| EH-006          | 🟢 Optional  | `apps/runner/src/lib/opencode.mjs:772`              | AbortError detection is a string match on `err.name`; SDK's real abort surface unverified          | Leave as-is  |
| EH-008          | 🟢 Optional  | `apps/runner/src/lib/opencode.mjs:764`              | SDK path forwards only the API key; subprocess path scrubs a wider allowlist (asymmetry)         | Leave as-is  |
| SP-003 / SP-004 / SP-007 | 🟢 Optional | `apps/runner/src/lib/opencode.mjs:752` + `openrouter.mjs:67` | Test seam + per-call SDK construction are fine for one-Job-one-call today         | Leave as-is  |
| RD-005 / RD-008 / RD-009 | 🟢 Optional | `openrouter.mjs:220`, `openrouter.test.mjs:201`, `opencode.test.mjs:797` | Test names + dual-mode signatures are acceptable     | Leave as-is  |

**Counts:** 🔴 Blocking 0 · 🟡 Follow-up 12 (5 change-now + 7 defer) · 🟢 Optional 7.
**Out of scope (pre-existing, not flagged against this PR):** `// temporary verify mark 1785624554` at `apps/runner/src/lib/opencode.mjs:821` (added in commit `e3bce86`, three days before this PR, in a chore to test image-digest sync — flagged in `audits/design-pattern.md` as DP-001, demoted here per the skill's "Check scope" rule).

---

## Categorized Findings

### Code Quality (CQ-*)
- **CQ-001** (Defer): `runOpenRouterSkill` is 105 lines doing five jobs; CC is 7-8 (under threshold) but cognitive load is real. Split the error-translation tail into a helper. (Also: SP-001)
- **CQ-003** (Defer): Catch block at `opencode.mjs:766-794` interleaves `isAbort` classification, errlog, and a `parseReviewOutput("") || "sdk call failed"` whose `||` branch is dead because the parser always sets `parseError`. (Also: SP-005)
- **CQ-004** (Change now): `readOpencodeModel` is exported but not directly tested. On the QUB-98 cutover path. (Also: RD-004, EH-002)
- **CQ-005** (Change now): The "no model configured" throw is uncovered. (Also: EH-007)
- **CQ-002, CQ-006, CQ-007, CQ-008, CQ-009**: minor cohesion / test-coverage / name-clarity items. None blocking.

### Structural Choices (DP-*)
- **DP-002** (Change now): The `// cross-module surface is just one call to callOpenRouter and one call to buildTelemetry` comment in `opencode.mjs:712-715` is wrong — the file imports three symbols and the *real* constraint is reusing `buildBoopPrompt` + `parseReviewOutput` from the same file. (Also: RD-003, SP-006)
- **DP-003** (Defer): The PR body says "QUB-98 deletes the opencode CLI, ConfigMap, and PTY wrap." It also requires renaming `runOpenCodeSkill` → `runReviewSkill` (or similar), moving `runOpenRouterSkill` out of `opencode.mjs`, and dropping the `opencode-ai` dep. Add this to the QUB-98 handoff so the next author doesn't underestimate the work.
- **DP-004, DP-005**: per-call SDK client + missing model-source log. Fine today.

### Error Handling (EH-*)
- **EH-001** (Change now): `sdk flag resolved` is logged at `slog.LevelDebug`. The receiver's default `LOG_LEVEL` falls through to `LevelInfo`. During a per-PR rollout, an operator investigating a misrouted PR won't see the decision without restarting the receiver with `LOG_LEVEL=debug`. Promote to `Info`.
- **EH-003** (Defer): When the SDK fails for any non-abort reason, the PR status comment collapses to `parseError: "sdk call failed"` and the real `err.message` only lands in `telemetry.error`. When the dashboard data layer is off (the default for clusters without `RUNNER_TOKEN` set), the PR author loses all triage detail. Two reasonable fixes in `audits/error-handling.md` (enrich parseError with HTTP status, or add a `parseErrorDetail` field).
- **EH-005** (Defer): `resolveSDKEnabled` accepts any non-empty string from `BOOP_USE_OPENROUTER_SDK` and only treats `"1"` as truthy. An operator who sets `BOOP_USE_OPENROUTER_SDK=true` (Helm convention) sees the cluster default flip to `"true"` in the log but the Job runs on the subprocess path. Add a startup warning for unexpected values.
- **EH-002, EH-004, EH-006, EH-007, EH-008, EH-009**: minor; `EH-009` is a positive note (the "does not read files or spawn processes" test pins the SDK module's surface as pure I/O).

### Readability (RD-*)
- **RD-001 / SP-008** (Defer): `submitJob` is 15 positional args; the test file needs `// reactionCommentID`, `// statusCommentID`, `// installationID` inline labels — evidence the call site is at the readability ceiling. The structural audit (SP-008) recommends a `submitJobInput` struct; the readability angle is the inline `//` comments. Not a QUB-94 change, but flag the next per-PR feature lands on a struct shape, not a 16th positional.
- **RD-002** (Defer): `BOOP_USE_OPENROUTER_SDK` string flag carries `"0"` / `"1"` literals through 5 sites in 2 languages with no shared symbol. One Go helper would close it.
- **RD-007** (Change now): Two QUB-94 comment blocks back-to-back in `runOpenCodeSkill` (lines 590-594 + 596-600). Trim the in-body one to the one piece of information not in the docstring (the delete target).
- **RD-003** (Change now, with DP-002 / SP-006): trim the "cross-module surface" sentence from the comment.
- **RD-004** (Change now, with CQ-004): `readOpencodeModel` lives in `openrouter.mjs` but reads `opencode.json`; the name doesn't signal that it's a transitional helper.
- **RD-005, RD-006, RD-008, RD-009**: minor (test names, mock shapes, dual-mode signatures). Leave as-is.
- **RD-010** (Defer, with DP-005): model-resolution block comment is the only place that names the source order; a one-line `modelSource` log field would be greppable.

### Structural & Dependency (SP-*)
- **SP-001** (Defer, with CQ-001): `runOpenRouterSkill` plays three roles.
- **SP-002** (Defer): the QUB-94 comment on `runOpenCodeSkill` says "the subprocess block below can be deleted in QUB-98." After the delete, the function still has the name `runOpenCodeSkill` even though it does not call opencode. Open a QUB-98 todo: rename `runOpenCodeSkill` → `runReviewSkill`.
- **SP-005** (Defer, with CQ-003): the dual error contract on the SDK call (throw on Abort, return degraded review on everything else) is correct but the comment at `opencode.mjs:768-771` only explains the AbortError case. Add a one-line comment above the `catch` describing the dual contract.
- **SP-006** (Change now, with RD-003 / DP-002): the "circular import" comment is wrong — `openrouter.mjs` does not import from `opencode.mjs`. The real constraint is that putting `runOpenRouterSkill` in `openrouter.mjs` would force `openrouter.mjs` to import `buildBoopPrompt` + `parseReviewOutput`, completing a cycle. Update the comment.
- **SP-008** (Defer, with RD-001): `submitJob` parameter list.
- **SP-003, SP-004, SP-007**: test seam, env-shaped key, per-call SDK construction. Fine today.

---

## Suggested PR Comments

### Comment 1 — Promote the SDK flag log
**File:** `apps/receiver/internal/webhook/handler.go:565` | **Tier:** 🟡 Follow-up | **Decide:** Change now

**Observation:** `sdk flag resolved` is logged at `slog.LevelDebug`. The receiver's default log level is `Info` (`main.go:138`), so during a per-PR rollout the operator investigating a misrouted PR won't see the decision without restarting the receiver with `LOG_LEVEL=debug`.

**Impact:** The flag is per-PR; this is a regression hazard during the QUB-94 rollout.

**Suggestion:**
```go
h.logger.Info("sdk flag resolved", "delivery", delivery, "value", sdkEnabled,
  "label_present", hasLabel(labels, sdkEnabledLabel),
  "cluster_default", h.cfg.OpenRouterSDKDefault)
```

The line is emitted once per review (low volume) and carries no secret material.

### Comment 2 — Trim the cross-module surface comment
**File:** `apps/runner/src/lib/opencode.mjs:712-715` | **Tier:** 🟡 Follow-up | **Decide:** Change now

**Observation:** The comment claims the cross-module surface is "one call to `callOpenRouter` and one call to `buildTelemetry`." The import block at the top of the file pulls three symbols from `openrouter.mjs` (`buildTelemetry`, `callOpenRouter`, `readOpencodeModel`), and the function body also calls `readOpencodeModel` at line 730.

**Impact:** Future readers who `grep` for "the cross-module surface" will miss the third import and the function's own model-resolution branch.

**Suggestion:** Drop the second sentence and tighten the constraint to the actual reason `runOpenRouterSkill` lives in `opencode.mjs`:
```js
// The function lives in opencode.mjs (not openrouter.mjs) because
// it reuses buildBoopPrompt + parseReviewOutput, and openrouter.mjs
// is meant to stay a leaf. Moving this function would force
// openrouter.mjs to import from opencode.mjs, completing a cycle
// (opencode.mjs already imports from openrouter.mjs).
```

### Comment 3 — Add the two uncovered tests
**File:** `apps/runner/src/lib/openrouter.test.mjs` + `apps/runner/src/lib/opencode.test.mjs` | **Tier:** 🟡 Follow-up | **Decide:** Change now

**Observation:** `readOpencodeModel` is exported but not directly tested, and the orchestrator's "no model configured" throw is uncovered. Both are on the QUB-98 cutover path (which deletes the ConfigMap the `readOpencodeModel` branch reads).

**Impact:** The first time `readOpencodeModel` fails in production — or the first time the throw fires after QUB-98 — is the first time the failure is tested.

**Suggestion:** Three small tests (full snippets in `audits/code-quality.md` CQ-004 + CQ-005). All three close gaps on the QUB-98 upgrade path.

### Comment 4 — Trim the duplicate QUB-94 comment in `runOpenCodeSkill`
**File:** `apps/runner/src/lib/opencode.mjs:585-600` | **Tier:** 🟡 Follow-up | **Decide:** Change now

**Observation:** Two QUB-94 comment blocks within ten lines of each other. The function-level docstring (590-594) already says "the function takes the in-process SDK path; the old TUI / JSON paths stay intact so a flag flip is the rollback." The in-body comment (596-600) repeats the same context.

**Impact:** A reader skims both and walks away with the same information twice.

**Suggestion:** Keep the function-level docstring. Trim the in-body comment to the one piece of new information — the delete target:
```js
if (ctx.openrouterSdkEnabled) {
  // SDK fast-path. The legacy branch below dies in QUB-98.
  return await runOpenRouterSkill(openrouterApiKey, ctx, deps);
}
```

### Comment 5 — Capture the QUB-98 rename in the handoff
**File:** PR description / `docs/development.md` | **Tier:** 🟡 Follow-up | **Decide:** Defer

**Observation:** The PR body frames QUB-98 as "deletes the opencode CLI, ConfigMap, and PTY wrap." After that delete, `runOpenCodeSkill` no longer calls opencode and `runOpenRouterSkill` is the only path — but it lives in `opencode.mjs` because of a circular-import constraint that the delete removes. QUB-98 also has to drop `opencode-ai` from `package.json` and `package-lock.json`, and rename `runOpenCodeSkill` → `runReviewSkill` (plus the `defaultRunOpenCodeSkill` caller in `workflow.mjs`).

**Impact:** If the QUB-98 author treats the work as a pure delete, the wrong-named function stays in the wrong-named file and "opencode" anchors the runner module name forever.

**Suggestion:** Add a one-line QUB-98 todo: "rename `runOpenCodeSkill` → `runReviewSkill`, move into the OpenRouter module, delete `opencode.mjs` / `opencode_json.mjs` / `materializeConfig` / `runOpencode` / `importRunOpencodeJSON` / `shellQuote` / `opencode-ai` dep."

---

## What's Working Well

1. **The feature-flag seam is at the right layer.** The Go receiver resolves the cluster default + per-PR label into a single string env var; the runner just branches on a boolean and does not know about labels. The `resolveSDKEnabled` test table at `handler_test.go:179-207` covers the truth table cleanly (8 cases, including case-insensitive label match and unset default). `apps/receiver/internal/webhook/handler.go:815-835` is the cleanest new code in the PR.

2. **The SDK boundary is clean.** `@openrouter/sdk` is imported in exactly one file (`apps/runner/src/lib/openrouter.mjs`); the `extractAssistantText` / `extractUsage` helpers normalise the SDK's `promptTokens` / `promptTokensDetails.cachedTokens` / `completionTokensDetails.reasoningTokens` (camelCase, nested) into the runner's `prompt_tokens` / `cached_tokens` / `reasoning_tokens` (snake_case, flat) before anything outside the module sees them. The "does not read files or spawn processes" test at `openrouter.test.mjs:201-229` is a load-bearing guard that pins the boundary — if a future change accidentally introduces `fs.readFile` or `child_process.spawn` into the SDK path, the test fails loudly. Defense-in-depth worth keeping.

3. **The QUB-98 deletion is mostly a one-file edit.** The conditional dispatch is a single early `if (ctx.openrouterSdkEnabled)` at `opencode.mjs:595`; every subprocess-side function (`materializeConfig`, `runOpencode`, `importRunOpencodeJSON`, `shellQuote`, the JSON-mode branch) sits in one file, below the SDK branch's `if/return`. The new SDK path reuses `parseReviewOutput` so `postReview` / `postInlineComments` are unchanged. The author kept the rollback path symmetric: `BOOP_USE_OPENROUTER_SDK=0` is the opencode subprocess, and a flag flip is the rollback — no code change, no Job re-create, no migration.

---

## Sub-Agent Output Files

- `audits/code-quality.md` (9 findings, 0 🔴 / 5 🟡 / 4 🟢)
- `audits/design-pattern.md` (5 findings, 0 🔴 / 3 🟡 / 2 🟢)
- `audits/error-handling.md` (9 findings, 0 🔴 / 3 🟡 / 5 🟢 + 1 positive)
- `audits/readability.md` (10 findings, 0 🔴 / 4 🟡 / 6 🟢)
- `audits/solid-principles.md` (8 findings, 0 🔴 / 4 🟡 / 4 🟢)
- `audits/pr-review-summary.md` (this file)

**Cross-audit dedup applied.** The 5 change-now items in the priority table are each the merged result of 2-3 sub-agent findings pointing at the same location for related reasons. Tier-🔴-Blocking findings re-verified: 0.
