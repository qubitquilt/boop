# Structural Review
**Date:** 2026-08-04
**Scope:** QUB-94 — `feature/qub-94-migrate-boop-runner-from-opencode-cli-to-openrouter-sdk`

Files in scope:
- `apps/receiver/cmd/receiver/main.go`
- `apps/receiver/internal/webhook/handler.go`
- `apps/receiver/internal/webhook/handler_test.go`
- `apps/receiver/internal/webhook/jobbuilder.go`
- `apps/runner/src/lib/config.mjs`
- `apps/runner/src/lib/opencode.mjs`
- `apps/runner/src/lib/openrouter.mjs`
- `apps/runner/src/lib/openrouter.test.mjs`
- `apps/runner/src/lib/opencode.test.mjs`
- `docs/development.md`

---

## Summary

The seam is in the right place: the receiver resolves the cluster-default + per-PR label into a single `BOOP_USE_OPENROUTER_SDK` string, the runner just reads it as a boolean and dispatches. The SDK boundary is clean — `@openrouter/sdk` is imported in exactly one file, `openrouter.mjs`, and everything downstream sees a normalised `{ text, usage, model }` shape. The conditional dispatch is the only friction: a `// temporary verify mark` artifact slipped into `opencode.mjs:821`, the cross-module surface is wider than the comment claims, and the post-QUB-98 cleanup will be a rename + file move, not the pure delete the comment promises.

---

## Findings

### DP-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Change now                                         |
| Location | `apps/runner/src/lib/opencode.mjs:821` — file tail |

**Observation:** The last line of `opencode.mjs` is `// temporary verify mark 1785624554`. It is not documentation — it is a numeric marker unrelated to any token in the file, and it sits after the closing `}` of `runOpenRouterSkill` with no preceding code.

**Impact:** Left in the tree it ships to production. Greppable by anyone with the file open. Cheap to fix today, embarrassing to leave for QUB-98 to clean up alongside the broader dead-path removal.

**Suggestion:** Delete the line.

---

### DP-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Change now                                         |
| Location | `apps/runner/src/lib/opencode.mjs:712-715` and `apps/runner/src/lib/opencode.mjs:12-16` |

**Observation:** The comment on `runOpenRouterSkill` says:

> "The cross-module surface is just one call to `callOpenRouter` and one call to `buildTelemetry`."

But the import block at the top of the file pulls three symbols from `./openrouter.mjs`:

```js
import {
  buildTelemetry,
  callOpenRouter,
  readOpencodeModel,
} from "./openrouter.mjs";
```

`readOpencodeModel` is used in the model-resolution branch of `runOpenRouterSkill` itself (lines 728–731), not from a caller outside this function. The comment is still *technically* true if "cross-module surface" means "calls inside `runOpenRouterSkill`" — but it is misleading on a casual read because the import list and the function's own body disagree with it.

**Impact:** Future readers who grep for "the cross-module surface" will miss the third import. After QUB-98 removes `readOpencodeModel`, the comment becomes accurate — but the cost of leaving it wrong now is one confused reviewer per re-read.

**Suggestion:** Either widen the comment to mention the `readOpencodeModel` call, or move `readOpencodeModel` into the model-resolution layer that already lives closer to `buildBoopPrompt` (so the SDK module only owns the LLM call + telemetry). Either way, the comment should match the imports.

---

### DP-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/runner/src/lib/opencode.mjs:585-820` (whole `runOpenCodeSkill` + `runOpenRouterSkill`) |

**Observation:** The QUB-94 comment on `runOpenCodeSkill` says:

> "once the cutover ships the subprocess block below can be deleted in QUB-98."

The delete is mechanical — every subprocess-side function (`materializeConfig`, `runOpencode`, `importRunOpencodeJSON`, `shellQuote`, the JSON-mode branch) sits in one file, below the SDK branch's `if/return`. But the post-cutover shape is awkward:

1. `runOpenCodeSkill` becomes a one-liner that delegates to `runOpenRouterSkill`. Its name is now misleading (it does not call opencode at all), and the only way to fix the name is to also relocate the function.
2. `runOpenRouterSkill` is the only path after the cutover, but it lives in `opencode.mjs` because the circular-import comment (`openrouter.mjs` → `opencode.mjs` for `buildBoopPrompt`/`parseReviewOutput`, plus `opencode.mjs` → `openrouter.mjs` for the SDK helpers) forbids the move *today*. After QUB-98 the constraint goes away, but moving the function is no longer a "delete"; it is a rename + relocation.
3. `opencode-ai` stays in `package.json` even though the SDK path does not use it; QUB-98 must also drop the dep + `package-lock.json` entry.
4. The `runOpenRouterSkill` comment explicitly says it lives in `opencode.mjs` "to avoid a circular import" — that justification disappears with the dead path, so the file layout inherited from the rollout becomes the wrong layout for the post-cutover world.

**Impact:** The PR description frames QUB-98 as "deletes the opencode CLI, ConfigMap, and PTY wrap." It will also have to rename `runOpenCodeSkill` (probably to `runReview` or `runBoopReview`) and move `runOpenRouterSkill` into a file that is *not* `opencode.mjs`. If the author of QUB-98 treats the work as a pure delete, they will leave the wrong-named function in the wrong file and the runner module name will keep saying "opencode" forever.

**Suggestion:** Add a QUB-98 line item: "rename `runOpenCodeSkill` → `runReview`, move into the OpenRouter module, delete `opencode.mjs` / `opencode_json.mjs` / `materializeConfig` / `runOpencode` / `importRunOpencodeJSON` / `shellQuote` / `opencode-ai` dep." Capture it in the PR description's QUB-98 handoff so the next author doesn't underestimate the work.

---

### DP-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/openrouter.mjs:67` — `callOpenRouter` |

**Observation:** `OpenRouter` client is constructed per call:

```js
const client = injectedClient ?? new OpenRouter({ apiKey });
```

There is no module-level singleton. The test seam (`deps.client` injection) is preserved and the runner is one-shot per Job, so the per-call cost is negligible.

**Impact:** Fine today. Becomes a real friction point only if a future change introduces a long-lived runner that issues many reviews in one process (e.g. an HTTP server wrapper, a retry loop) — at that point the lack of a singleton means re-establishing HTTP keepalive connections and re-reading the API key on every call. No current consumer hits this.

**Suggestion:** Leave as-is. If a multi-call path lands, extract a `getOpenRouterClient(apiKey)` factory at module scope and inject it into `callOpenRouter` like the test does.

---

### DP-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/opencode.mjs:721-736` — model resolution in `runOpenRouterSkill` |

**Observation:** Model resolution is a two-step chain — `ctx.openrouterModel` (env override) takes priority; if empty, `readOpencodeModel(deps)` reads `opencode.json` from the ConfigMap mount; if both are empty, the function throws. There is no log line that says which source won. The pre-QUB-98 world needs both (ConfigMap is still mounted); the post-QUB-98 world needs only the env path.

**Impact:** When the SDK path picks up the wrong model, an operator has to read `readOpencodeModel` + the env to figure out which one won. The branch is small and the resolution is deterministic, so this is a minor cost.

**Suggestion:** One log line at the chosen-source branch — `{ source: model ? "env" : "configmap", model }` — would close the gap. Defer until someone debugs a "wrong model on the wrong PR" issue.

---

## Notes (not findings)

- **SDK boundary is clean.** `grep -l '@openrouter/sdk' apps/runner/src/` returns only `openrouter.mjs`. The `extractAssistantText` / `extractUsage` helpers in `openrouter.mjs` normalise the SDK's `promptTokens` / `promptTokensDetails.cachedTokens` / `completionTokensDetails.reasoningTokens` (camelCase, nested) into the runner's `prompt_tokens` / `cached_tokens` / `reasoning_tokens` (snake_case, flat) before anything outside the module sees them. The test in `openrouter.test.mjs` that asserts "no fs, no spawn" is the right shape of guard for that boundary.
- **Feature-flag seam is at the right layer.** The receiver does cluster-default + per-PR-label resolution and forwards a single string env var. The runner does not know about labels, does not know about the cluster default, and just branches on a boolean. The Go side carries the policy; the Node side is dumb. This is the correct shape for a feature flag, and the `resolveSDKEnabled` unit test covers the truth table cleanly (8 cases, including case-insensitive label match and unset default).
- **Telemetry envelope is consistent.** Both paths return `{ ...review, telemetry }` so `postReview` / `postInlineComments` are unaffected. The SDK path always returns a telemetry object; the subprocess TUI path returns `telemetry: null`. Existing consumers already null-check (the JSON path was the only populated case before QUB-94), so this is not a new contract.

---

## Unable to verify

- **Long-term shape of the SDK client lifecycle** — depends on whether the QUB-94 → QUB-98 sequence lands cleanly. Cannot test the rename/move without a second PR.
- **Whether `runOpenRouterSkill` should live in a file named after the OpenRouter provider or after the runner's role** — naming is a judgement call, not a structural one; deferred.
