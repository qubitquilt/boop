# Structural & Dependency Audit
**Date:** 2026-08-04
**Scope:** QUB-94 — OpenRouter SDK migration. Files: `apps/receiver/cmd/receiver/main.go`, `apps/receiver/internal/webhook/handler.go`, `apps/receiver/internal/webhook/handler_test.go`, `apps/receiver/internal/webhook/jobbuilder.go`, `apps/runner/src/lib/config.mjs`, `apps/runner/src/lib/opencode.mjs`, `apps/runner/src/lib/opencode.test.mjs`, `apps/runner/src/lib/openrouter.mjs` (new), `apps/runner/src/lib/openrouter.test.mjs` (new).

---

## Summary

The migration is shaped well for the rollback: the SDK fast-path is a single early `if` in `runOpenCodeSkill`, the rest of the file is the unchanged subprocess branch, and the QUB-98 delete is mostly a one-file edit. Flag layering (cluster env → per-PR label → `BOOP_USE_OPENROUTER_SDK` env → boolean `ctx.openrouterSdkEnabled`) is clean and the receiver does not leak flag logic into the runner. The new file `openrouter.mjs` is a clean leaf: nothing in the repo imports it, and it does not import from `opencode.mjs`, so the cross-file surface is genuinely one-directional. The main coupling risk is that `runOpenRouterSkill` (~100 lines) plays three roles (prompt orchestrator, SDK transport, error→review translator) and would balloon if a future change adds streaming, tool use, or retries.

---

## Findings

### SP-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/runner/src/lib/opencode.mjs:716-820` — `runOpenRouterSkill` |

**Observation:** `runOpenRouterSkill` is one function that builds the prompt, resolves the model, posts status, calls the SDK, translates `AbortError` into a timeout throw, translates other errors into a degraded `parseError` review, parses the response, builds telemetry, and logs both the start and exit. Describing the function requires three different verbs ("orchestrate," "call," "translate"). It is not currently causing a bug, and the function is short enough to read in one pass.

**Impact:** QUB-98 keeps this function intact, so the cost is only future work. If a follow-up adds streaming responses, tool use, or a retry loop, the function will exceed 200 lines and the error-translation branch (lines 766-794) will become the thing that nobody wants to touch. The model-resolution block (lines 728-736) is also the part that will change for QUB-98: the `readOpencodeModel` branch dies when the ConfigMap goes away, and the operator message ("set OPENROUTER_MODEL or mount opencode.json") becomes wrong. Splitting that block out now makes the QUB-98 edit a one-line delete.

**Suggestion:**
```js
// Move model resolution to a dedicated function next to readOpencodeModel.
export function resolveOpenRouterModel(ctx, deps) {
  const fromCtx = ctx.openrouterModel;
  if (fromCtx) return fromCtx;
  return readOpencodeModel(deps);
}

// runOpenRouterSkill then drops lines 728-736 and calls
// const model = await resolveOpenRouterModel(ctx, deps);
```

---

### SP-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer (until QUB-98) |
| Location | `apps/runner/src/lib/opencode.mjs:595-603` — `runOpenCodeSkill` dispatch |

**Observation:** The dispatch is a single early `if (ctx.openrouterSdkEnabled) return await runOpenRouterSkill(...)`. After QUB-98 deletes the subprocess branch, the function still has the name `runOpenCodeSkill` even though it does not run opencode. The orchestrator caller (`workflow.mjs:729`) is also named `defaultRunOpenCodeSkill`.

**Impact:** This is a naming-only concern and does not block QUB-94. The function body, the export name, and the caller all keep the opencode-anchored wording through the cutover. A maintainer reading the post-cutover file has to read the body to discover the function no longer shells out to opencode. Renaming is a search-and-replace in `opencode.mjs` + `workflow.mjs` + ~15 test files; that is the kind of refactor that's best done as part of the QUB-98 commit so the rename is a single PR.

**Suggestion:** Open a QUB-98 todo: rename `runOpenCodeSkill` → `runReviewSkill` (or `runNarrateStep`), and `defaultRunOpenCodeSkill` in `workflow.mjs` accordingly. Do not do it in QUB-94 — the rename would touch files QUB-94 has no reason to touch.

---

### SP-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/opencode.mjs:752` — `deps.callOpenRouter || callOpenRouter` |

**Observation:** `runOpenRouterSkill` reads `deps.callOpenRouter` as a test injection point. The production `deps` bundle built in `index.mjs:127-130` does not set it, so the real `callOpenRouter` is used. The test seam mirrors the existing pattern (`deps.runOpencodeJSON`, `deps.spawnFn`, `deps.fetchImpl`).

**Impact:** The override accidentally widens `deps` into something that *can* carry a stub for the SDK call, even though no production caller wires one up. If a future operator wires `deps` from config, they could pass a `callOpenRouter` field and route around the real SDK. Low risk — the field is not in the public dep surface documented anywhere, and `index.mjs` does not accept it from overrides for this path. The pattern matches the rest of the lib.

**Suggestion:** Add a one-line comment at the top of `opencode.mjs` near the import block noting that `deps.callOpenRouter` is a test seam and `index.mjs` never sets it. No code change.

---

### SP-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/openrouter.mjs:44-67` — `callOpenRouter` env-shaped key |

**Observation:** The API key reaches `callOpenRouter` via `deps.env.OPENROUTER_API_KEY`, not as a direct argument. The production call site (`opencode.mjs:754-765`) builds a single-key env object `{ OPENROUTER_API_KEY: openrouterApiKey }` to forward the key. The SDK constructor at `openrouter.mjs:67` reads `env.OPENROUTER_API_KEY` and passes it to `new OpenRouter({ apiKey })`.

**Impact:** This is mildly indirect (two indirections to pass one string) but it matches the SDK's own env-shaped contract: `new OpenRouter({ apiKey })` is the only documented constructor, and the SDK reads the same env var internally. Forcing a direct `apiKey` argument would require a new constructor call site but would not simplify anything. The shape also keeps `callOpenRouter` symmetric with how tests inject keys (via `env`). No change recommended.

**Suggestion:** No change. If a future SDK release adds a `client` constructor that takes the key directly, fold the env round-trip at that point.

---

### SP-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/runner/src/lib/opencode.mjs:766-794` — error handling in `runOpenRouterSkill` |

**Observation:** The `try` block in `runOpenRouterSkill` has two distinct error contracts on a single call site:
1. `AbortError` (timeout) → `throw new Error("openrouter run exceeded …-min timeout")` — propagates up to the runner's outer catch.
2. Any other error (4xx, 5xx, network, JSON parse of response) → `return { ...review, telemetry: buildTelemetry(null, err) }` where `review.parseError = "sdk call failed"`.

The caller (`narrateSubStage` in `workflow.mjs:708-734`) reads `state.review.summary` to decide whether to post, and reads `state.review.telemetry` to decide whether to POST telemetry. The split means a 4xx looks like a parse failure downstream (no throw) while a timeout looks like a system error (throw). Both are reasonable, but the dual contract is easy to miss.

**Impact:** The runner's existing parse-failure path already handles the "return a degraded review" shape, so contract #2 reuses an existing flow. The test `runOpenCodeSkill returns empty telemetry on SDK call failure` covers #2; the test `runOpenCodeSkill throws when the SDK call is aborted` covers #1. Coverage is fine. The concern is purely readability — a future maintainer who adds a new error type to the SDK has to know which of the two contracts applies. Today that decision is correct (`AbortError` is the only one tied to a runtime deadline), but the comment at line 768-771 only explains the AbortError case.

**Suggestion:** Add a one-line comment above the `catch (err) {` line describing the dual contract:

```js
// Two error contracts on one call site:
//   AbortError (timeout) — throw; the runner's outer catch handles it.
//   Anything else — return a degraded review; the parse-failure path
//   downstream swallows the summary and skips the post.
```

---

### SP-006
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/opencode.mjs:712-715` — circular-import comment |

**Observation:** The comment claims `runOpenRouterSkill` lives in `opencode.mjs` (not `openrouter.mjs`) "to avoid a circular import." Reading `openrouter.mjs` confirms it does NOT import from `opencode.mjs` — its only imports are `@openrouter/sdk` and `./config.mjs`. There is no cycle to avoid today. The real reason is that `runOpenRouterSkill` uses `buildBoopPrompt` and `parseReviewOutput`, which live in `opencode.mjs`. If `runOpenRouterSkill` lived in `openrouter.mjs`, `openrouter.mjs` would need to import those two helpers, and `opencode.mjs` already imports from `openrouter.mjs` (for `callOpenRouter`, `buildTelemetry`, `readOpencodeModel`). That would create the cycle.

**Impact:** Misleading comment. A reader who skims the file may waste time looking for an existing cycle or assume the cross-file surface is more tangled than it is. The actual layering (`openrouter.mjs` is a leaf; `opencode.mjs` calls into it) is the right shape, and the comment as written does not describe it.

**Suggestion:** Tweak the comment to describe the actual constraint:
```js
// runOpenRouterSkill lives in opencode.mjs (not openrouter.mjs)
// because it reuses buildBoopPrompt + parseReviewOutput, and
// openrouter.mjs is meant to stay a leaf (only @openrouter/sdk
// and config.mjs). Putting this function in openrouter.mjs would
// force openrouter.mjs to import from opencode.mjs, completing
// a cycle (opencode.mjs already imports from openrouter.mjs).
```

---

### SP-007
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional |
| Decide   | Leave as-is |
| Location | `apps/runner/src/lib/openrouter.mjs:67` — `new OpenRouter({ apiKey })` per call |

**Observation:** `callOpenRouter` constructs the SDK client per invocation: `const client = injectedClient ?? new OpenRouter({ apiKey })`. In this codebase the runner is a one-shot K8s Job (`index.mjs:115-281` runs one review then `process.exit(1)` on failure or returns on success — no retry loop), so the SDK is constructed at most once per Job.

**Impact:** Per-call construction is fine today. If a future change calls the SDK multiple times per Job (e.g. multi-expert narrate, re-review delta, classifier call), the construction cost compounds and an injected client becomes more useful. The `injectedClient` seam is already in place for that day. No change now.

**Suggestion:** No change. If a follow-up starts making multiple SDK calls per Job, move the client construction to `index.mjs` (in the `makeDeps` bundle) and pass it through `deps.client`.

---

### SP-008
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up |
| Decide   | Defer |
| Location | `apps/receiver/internal/webhook/handler.go:557` — `submitJob` parameter list |

**Observation:** `submitJob` now has 15 positional parameters (the diff added `labels []string` as the 15th). The signature was already long before QUB-94; the new label is a per-PR signal that will likely be followed by more (e.g. `boop:skip-classify`, `boop:dry-run`). Each future label will append another parameter to a function that is already hard to read at a glance.

**Impact:** This is the receiver-side analogue of the runner's "narrate stage" pulling more decisions. The labels are not the only per-PR data — `pr.Labels` is read fresh from the webhook payload, and the `prMeta` struct at line 782-790 already carries `Labels []string`. The function could take `prMeta` (or a smaller struct that wraps the PR data) instead of unpacking each field. Not blocking, but the next per-PR signal will make the change more expensive.

**Suggestion:** Not a QUB-94 change. Open a follow-up to migrate `submitJob` to a struct-shaped input:
```go
type submitJobInput struct {
    Delivery         string
    Owner, Repo      string
    Number           int
    HeadSHA, BaseRef, PreviousHeadSHA string
    Reason           string
    ReactionCommentID, StatusCommentID, InstallationID int64
    ReviewNumber     int
    Labels           []string
}
```

---

## Structural Snapshot

```
                ┌──────────────────────┐
                │  receiver (Go)       │
                │  Config.OpenRouter-  │
                │  SDKDefault (env)    │
                │  +                  │
                │  resolveSDKEnabled()│
                │  (label + default)  │
                └──────────┬───────────┘
                           │ templateVars.OpenRouterSDKEnabled ("0"|"1")
                           ▼
                ┌──────────────────────┐
                │  buildJob (Go)       │
                │  → BOOP_USE_OPEN-   │
                │    ROUTER_SDK env    │
                └──────────┬───────────┘
                           │ K8s Job env
                           ▼
                ┌──────────────────────┐
                │  runner (Node)       │
                │  loadConfig()        │
                │  ctx.openrouter-     │
                │  SdkEnabled (bool)   │
                │  ctx.openrouterModel │
                └──────────┬───────────┘
                           │
                           ▼
       ┌─────────────────────────────────────┐
       │  runOpenCodeSkill (opencode.mjs)    │
       │                                     │
       │  if (ctx.openrouterSdkEnabled)      │
       │    → runOpenRouterSkill (here)      │
       │  else                               │
       │    → materializeConfig              │
       │    → runOpencode / JSON             │
       │                                     │
       │  runOpenRouterSkill uses:           │
       │    buildBoopPrompt (this file)      │
       │    parseReviewOutput (this file)    │
       │    callOpenRouter (openrouter.mjs)  │
       │    buildTelemetry (openrouter.mjs)  │
       │    readOpencodeModel(openrouter.mjs)│
       └─────────────────────────────────────┘
                           │
                           ▼
       ┌─────────────────────────────────────┐
       │  openrouter.mjs (leaf)              │
       │  - callOpenRouter                   │
       │    new OpenRouter({ apiKey })       │
       │    per call (one Job = one call)    │
       │  - buildTelemetry                   │
       │  - emptyTelemetry                   │
       │  - readOpencodeModel                │
       │  imports: @openrouter/sdk, config   │
       │  (no cycle today)                   │
       └─────────────────────────────────────┘
```

**Test seams:** `deps.callOpenRouter` (SDK stub), `deps.runOpencodeJSON` (subprocess JSON), `deps.spawnFn` (subprocess TUI), `deps.fetchImpl` (HTTP), `deps.fs` / `deps.execFile` (FS). `index.mjs` only wires the production ones; tests override per-case.

**What is easy to test:** the SDK path (fake `callOpenRouter` returns canned response), telemetry shape (`buildTelemetry` is pure), the receiver-side flag resolution (table-driven, no K8s). **What is hard to test:** the `injectedClient` seam in `callOpenRouter` (tested in `openrouter.test.mjs` only) and the dual error contract in `runOpenRouterSkill` (covered but easy to break silently). The runner lifecycle is one Job = one review, so no concurrency seams to worry about.
