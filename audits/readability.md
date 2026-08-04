# Readability Audit
**Date:** 2026-08-04
**Scope:** QUB-94 — `feature/qub-94-migrate-boop-runner-from-opencode-cli-to-openrouter-sdk`

Files reviewed (changed code only):
- `apps/receiver/cmd/receiver/main.go` (+7)
- `apps/receiver/internal/webhook/handler.go` (+56/-3) — new `resolveSDKEnabled`, `sdkEnabledLabel`, expanded `submitJob`
- `apps/receiver/internal/webhook/handler_test.go` (+59)
- `apps/receiver/internal/webhook/jobbuilder.go` (+16/-7)
- `apps/runner/src/lib/config.mjs` (+13)
- `apps/runner/src/lib/opencode.mjs` (+138/-4) — added `runOpenRouterSkill` ~120 lines
- `apps/runner/src/lib/opencode.test.mjs` (+83)
- `apps/runner/src/lib/openrouter.mjs` (+296, NEW FILE)
- `apps/runner/src/lib/openrouter.test.mjs` (+306, NEW FILE)

---

## Summary

The PR is readable on the whole: the new `openrouter.mjs` is a clean leaf with one verb-per-function surface (`callOpenRouter`, `buildTelemetry`, `readOpencodeModel`), the `resolveSDKEnabled` test table is the right shape, and the new tests name themselves after the behavior they pin. Two readable-but-fragile patterns stand out: (1) `submitJob` is now a 15-positional-argument function and the new call sites are comma-counted calls with no inline labels — adding the next per-PR signal will cost more than this one did; (2) the `"0"` / `"1"` string flag for the SDK path repeats in five locations across two languages with no shared symbol, and the `BOOP_USE_OPENROUTER_SDK` env-var name is repeated five times in comments alone. Neither is a defect today, but both will become the next misread if not addressed. The strongest positive: the new file's JSDoc and the receiver's docstring on `resolveSDKEnabled` explain *why* (rollout sequencing, opt-in semantics) rather than *what*, and the function bodies are short enough to confirm the docstring in one read.

---

## Findings

### RD-001
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/internal/webhook/handler.go:557` — `submitJob` signature; call sites at `:335` and `:420` |

**Observation:** `submitJob` now takes 15 positional parameters (the 15th, `labels []string`, is the new one). At the call sites the parameter list is read like a comma-counted row of values:

```go
h.submitJob(ctx, w, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, previousHeadSHA, fmt.Sprintf("pull_request.%s", pr.Action), 0, statusID, installationID, reviewNumber, pr.Labels)
h.submitJob(ctx, w, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, previousHeadSHA, fmt.Sprintf("issue_comment.by=%s", ic.SenderLogin), ic.CommentID, statusID, installationID, reviewNumber, nil)
```

The test call at `handler_test.go:834-850` adds a 15-line argument list with `// previousHeadSHA`, `// reactionCommentID`, `// statusCommentID`, `// installationID`, `// reviewNumber`, `// labels` inline comments — the comments exist *because* the call site is unreadable without them. The signature is well past the threshold where named fields at the call site help more than named parameters do.

**Impact:** Adding the next per-PR signal (e.g. a `dryRun` flag, a `requestReviewer` hint, a second opt-in label) will append another parameter, and the existing call sites will need a 16-argument line. The issue_comment path already has to pass `nil` for `labels` at the end of the list (`:420`), which is a smell: `nil` is indistinguishable from a future "no override" argument that happens to land at the same position. The Go test file's inline `// ...` comments at `:843-849` are evidence the call site is at the readability ceiling.

**Suggestion:** Move the per-PR inputs to a struct and let the two call sites build it inline. The PR is not the place to land this — the bigger refactor affects `submitJob`, both callers, and the two test sites — but flag it so the next per-PR feature lands on a `submitJobInput` shape, not a 16th positional. A minimal change that defers the larger refactor is to pull `labels` into the existing `prMeta` shape (already declared at `handler.go:782-790` with `Labels []string`) and pass `prMeta` instead of the flat list. The receiver code at `handler.go:320-336` already constructs a `prMeta` from the webhook payload.

```go
// Before (current PR)
func (h *Handler) submitJob(ctx context.Context, w http.ResponseWriter, delivery, owner, repo string, number int, headSHA, baseRef, previousHeadSHA, reason string, reactionCommentID, statusCommentID, installationID int64, reviewNumber int, labels []string) { … }

// After (deferred)
type submitJobInput struct {
    Delivery           string
    Owner, Repo        string
    Number             int
    HeadSHA, BaseRef   string
    PreviousHeadSHA    string
    Reason             string
    ReactionCommentID  int64
    StatusCommentID    int64
    InstallationID     int64
    ReviewNumber       int
    PR                 prMeta // carries Labels, future fields
}
func (h *Handler) submitJob(ctx context.Context, w http.ResponseWriter, in submitJobInput) { … }
```

Note: the structural audit (`audits/solid-principles.md`, SP-008) makes the same observation. The readability angle is the test file's inline `//` comments — those exist because the call site is unreadable today.

---

### RD-002
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/receiver/cmd/receiver/main.go:57`, `apps/receiver/internal/webhook/handler.go:91, 779, 831, 832`, `apps/runner/src/lib/config.mjs:129`, `apps/runner/src/lib/opencode.mjs:734` |

**Observation:** The `BOOP_USE_OPENROUTER_SDK` value is a stringly-typed flag carried through five locations:

1. Receiver `main.go:57` — `getenv("BOOP_USE_OPENROUTER_SDK", "0")` (default value)
2. `handler.go:91` — `Config.OpenRouterSDKDefault string` (storage)
3. `handler.go:779` — `templateVars.OpenRouterSDKEnabled string` (Job spec carrier)
4. `handler.go:828-833` — `resolveSDKEnabled` returns `"1"` / `"0"` after comparing `h.cfg.OpenRouterSDKDefault == "1"`
5. `jobbuilder.go:307` — `Value: v.OpenRouterSDKEnabled` (forwarded to Job env)
6. `config.mjs:129` — runner parses `env.BOOP_USE_OPENROUTER_SDK === "1"` (boolean coercion)
7. `opencode.mjs:734` — error message string "set OPENROUTER_MODEL or mount opencode.json" (different env var, but the prose reinforces the env-var-as-API)

The strings `"0"` and `"1"` appear as literals at sites 1, 4, 6, with no shared symbol. A reader tracing the flag's path from env-var to Job env has to find the four occurrences and confirm each by reading the surrounding code.

**Impact:** Today, the surface is small enough that a code search finds the right places. The cost shows up on the second change: when the env-var name moves (e.g. `BOOP_OPENROUTER_SDK_PATH` to match the new mount convention) or when a second value (e.g. `"dry-run"`) joins the flag, every site has to be hand-edited. The `error-handling.md` audit (EH-005) already flags the validation gap (any non-empty non-`"1"` string is silently treated as `"0"`); a shared helper would be the natural place to land the validation.

**Suggestion:** Add a `parseSDKFlag(value string) string` helper in `handler.go` (or `main.go` if the receiver splits the helper module), and use it in `resolveSDKEnabled` plus `main.go`'s `getenv` call:

```go
// One helper, two call sites:
func parseSDKFlag(v string) string {
    if v == "1" { return "1" }
    if v == "" || v == "0" { return "0" }
    // log warning at the call site if needed
    return "0"
}
```

For the runner side, `config.mjs:129` already uses `=== "1"` which is the local convention (the same file at line 112 uses `=== "1"` for `BOOP_SKIP_SKILL`). The Node side does not need a helper — the inline form is the codebase pattern. The readability win is on the Go side: one definition of "what does this string mean" instead of four.

---

### RD-003
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/opencode.mjs:704-715` — `runOpenRouterSkill` comment block (15 lines explaining the file placement) |

**Observation:** The function `runOpenRouterSkill` lives in `opencode.mjs`, not `openrouter.mjs`. The justification is spelled out in a 4-line block comment (lines 712-715):

```js
// The function lives in opencode.mjs (not openrouter.mjs) so it
// can reuse buildBoopPrompt + parseReviewOutput without a
// circular import. The cross-module surface is just one call to
// callOpenRouter and one call to buildTelemetry.
```

The comment is load-bearing — without it, a future reader would file a "this should be in `openrouter.mjs`" issue. The first sentence explains *why* (circular import) which is correct. The second sentence ("the cross-module surface is just one call to `callOpenRouter` and one call to `buildTelemetry`") is *what* the function does and is wrong on a literal reading — the function also calls `readOpencodeModel` (line 730) and `parseReviewOutput` (lines 788, 796). The structural audit (DP-002) calls this out.

**Impact:** The circular-import note is the right kind of comment (non-obvious decision, would otherwise be re-litigated). The "cross-module surface" sentence is misleading enough that a `grep` for it would miss the third import — a future reader maintaining this file will be confused for ~30 seconds. The cost is one stale sentence.

**Suggestion:** Trim the second sentence so the comment matches the import list:

```js
// The function lives in opencode.mjs (not openrouter.mjs) so it
// can reuse buildBoopPrompt + parseReviewOutput without a
// circular import.
```

Leave the rest. The circular-import note is exactly the kind of "why" comment the codebase benefits from (it is the same pattern as the comment on `runOpenCodeSkill` at lines 590-594, which also names the SDK path without describing the subprocess path's body).

---

### RD-004
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/runner/src/lib/openrouter.mjs:285-296` — `readOpencodeModel` |

**Observation:** The function is named `readOpencodeModel` and lives in `openrouter.mjs`, the OpenRouter SDK module. It reads from `opencode.json` (the *old* opencode CLI's config file) on the read-only ConfigMap mount. The name tells the reader the function reads the opencode model's config, but the file placement tells the reader this is part of the OpenRouter pipeline. The two are not contradictory today (it is a fallback source for the SDK path), but the name does not signal that this is a *legacy* / *transitional* helper that goes away in QUB-98.

**Impact:** A reader who arrives at `openrouter.mjs` from the call site `runOpenRouterSkill`'s comment ("`opencode.mjs` calls `readOpencodeModel` as a fallback for the SDK path") has to read the function to understand that "opencode" in the function name refers to `opencode.json` (the *file format*, not the opencode CLI). The relationship is two indirection hops (function in OpenRouter module, reads opencode CLI's config). After QUB-98 the function dies, so the cost is bounded.

**Suggestion:** Either rename the file-side helper to `readSdkModelFromConfigMap` (so the file it reads and the path it lives in both name "openrouter") or move the helper next to `buildBoopPrompt` in `opencode.mjs` (where the opencode.json mount is already used). The rename is a search-and-replace of one symbol; the relocation is bigger because the helper is exported. Both are deferrable to QUB-98 — the comment block on the function (`openrouter.mjs:267-280`) already documents the rollout intent.

---

### RD-005
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/openrouter.mjs:220-245` — `buildTelemetry(callResult, error)` signature |

**Observation:** `buildTelemetry` is invoked in two modes that the signature exposes but does not label: a successful call passes one argument (`buildTelemetry(callResult)` at `opencode.mjs:797`), a failed call passes two (`buildTelemetry(null, err)` at `opencode.mjs:793`). The function reads "build a telemetry object" without telling the reader the second argument is the error path's only input. The dual-purpose signature is also the only place in the file that takes two args.

**Impact:** A reader looking at `buildTelemetry(callResult)` at line 797 has to read the body to know that the second arg is optional and what it does. A reader looking at `buildTelemetry(null, err)` at line 793 has to confirm by reading the body that `null` is the right first arg (it is — the function checks `if (!callResult)` at line 222). The cost is one short re-read at each call site. Not blocking.

**Suggestion:** Leave as-is. The function is small (25 lines, all the logic is in one `if (!callResult)` branch) and the test file at `openrouter.test.mjs:290-298` pins both modes. Splitting into `buildSuccessTelemetry(callResult)` + `buildFailureTelemetry(err)` would be a rename and a small body change, but it adds a function for a 3-line branch. Not worth it.

---

### RD-006
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/opencode.test.mjs:710-722` — `makeSdkDeps` mock helper |

**Observation:** The mock helper at `opencode.test.mjs:702-722` returns a `deps` object whose `callOpenRouter` implementation has this return shape:

```js
return {
  text: callResult.text,
  usage: callResult.usage,
  model: callResult.model,
  ...opts,           // spreads the second argument (which contains env.OPENROUTER_API_KEY)
  _args: opts,       // re-records opts under a clearly-internal name
};
```

The `...opts` spread is invisible to the test assertions (the five SDK-branch tests assert on `text`, `usage`, `model`, `summary`, `confidence`, `inlineComments`, and `telemetry.*` — never on the spread fields). The `_args` field is never read either. The `error-handling.md` audit (EH-004) flags the `...opts` spread as a security smell: a future refactor that copies the "spread opts into the result" pattern into production code would leak the API key into the telemetry POST.

**Impact:** The `...opts` and `_args` lines are dead code in the test mock. They will confuse a future maintainer who is trying to understand what the mock returns. The cost is one re-read at the mock; the security risk is the audit's, not this one's.

**Suggestion:** Drop the two unused lines:

```js
return {
  text: callResult.text,
  usage: callResult.usage,
  model: callResult.model,
};
```

The mock is then exactly the shape production `callOpenRouter` returns (`openrouter.mjs:118-122`). If a future test wants to assert on the call args, add a closure-captured `sent = []` array, push to it, and assert on `sent` — same pattern `openrouter.test.mjs:17-25` already uses for its own `makeFakeClient`. Out of scope for this PR's correctness, but the mock is the right place to start.

---

### RD-007
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Change now                                         |
| Location | `apps/runner/src/lib/opencode.mjs:585-594` — `runOpenCodeSkill` JSDoc; `apps/runner/src/lib/opencode.mjs:590-594` and `apps/runner/src/lib/opencode.mjs:596-600` — two QUB-94 comments back-to-back |

**Observation:** `runOpenCodeSkill` has two QUB-94 comment blocks within ten lines of each other:

- Lines 590-594 (above the function, on the existing docstring): "QUB-94: when `ctx.openrouterSdkEnabled` is set, the function takes the in-process SDK path ... The old TUI / JSON paths stay intact so a flag flip is the rollback."
- Lines 596-600 (inside the function, above the `if`): "QUB-94: SDK fast-path. The flag defaults to false; production stays on the opencode subprocess until a week of clean runs passes on the SDK path. The branch is the entire difference between the two code paths — once the cutover ships the subprocess block below can be deleted in QUB-98."

The first block is a function-level docstring that says "the SDK path exists; the old path is the rollback." The second block is a statement-level comment that says "this `if` is the SDK fast-path; the subprocess block below is the delete target." Both are true. The function-level docstring is enough — the second block restates it inside the function body with a different phrasing.

**Impact:** A reader skimming `runOpenCodeSkill` for the first time reads both blocks and walks away with the same information twice. The second block is the one that explains the *delete target* (which is the operationally important thing) but the function-level docstring already says "the old TUI / JSON paths stay intact so a flag flip is the rollback." The repetition is not noise that obscures anything, but it adds 4-5 lines that say "QUB-94" again.

**Suggestion:** Keep the function-level docstring (it is the right place for the high-level "this function dispatches to one of two paths" note). Trim the inside-the-function comment to the one piece of information not already in the docstring — the delete target:

```js
if (ctx.openrouterSdkEnabled) {
  // SDK fast-path. The legacy branch below dies in QUB-98.
  return await runOpenRouterSkill(openrouterApiKey, ctx, deps);
}
```

The reader gets the rollout context (docstring) and the actionable note (delete target in the body) without re-reading the same paragraph twice.

---

### RD-008
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/openrouter.test.mjs:201-229` — `callOpenRouter does not read files or spawn processes` test |

**Observation:** The test name "does not read files or spawn processes" describes the negative space (what the function *must not* do) rather than the positive behavior. The body of the test does the right thing — sets up a fake `client.chat.send`, calls `callOpenRouter`, then iterates over recorded calls asserting none of them are `fs` / `spawn` / `execFile`. The name is a behavioural assertion in disguise: it pins the QUB-96 acceptance criterion that the SDK module has no I/O surface outside the SDK call.

**Impact:** The test name is the most behavior-descriptive available — "never calls fs or spawn" is a load-bearing guarantee that the module stays decoupled from the ConfigMap / subprocess world. A positive description ("calls only the SDK client") would miss the intent. Leave as-is. The only cost is that a `grep` for "calls SDK" or "sends chat" does not find this test, but a `grep` for "read files or spawn" does.

**Suggestion:** No change. This is a positive note — the test is unusual (it asserts the absence of side effects) and the name describes the guarantee accurately. If the codebase grew more "negative" tests in the future, consider a naming convention like `guard: <name> (<guarantee>)` to make them greppable; not needed at one occurrence.

---

### RD-009
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟢 Optional                                         |
| Decide   | Leave as-is                                        |
| Location | `apps/runner/src/lib/opencode.test.mjs:797-825` — `runOpenCodeSkill falls back to the legacy path when the flag is off` test |

**Observation:** The test at `opencode.test.mjs:797-825` exercises the legacy path by injecting a `deps.callOpenRouter` that throws a *unique* error (`"SDK_BRANCH_INVOKED"`) and then providing a `deps.fs` that throws a *different* unique error (`"legacy_path_reached"`) at the `materializeConfig` step. The assertion is that the function rejects with `/legacy_path_reached/`. The test name says "falls back to the legacy path" but the body of the test is "the SDK branch is *not* taken when the flag is off."

The test relies on a side effect of `materializeConfig` — it reads `opencode.json` and the mock throws on the `readFile` call. The assertion proves that `materializeConfig` was reached, which proves the SDK branch was skipped. The mechanism is correct, but the test name does not signal it.

**Impact:** A reader skimming the test names sees "falls back to the legacy path when the flag is off" and looks for an assertion that confirms a non-SDK behavior. The actual assertion is the rejection message, which is the only thing the test pins. The mechanism is sound but the test is *also* asserting the inverse ("the SDK branch is not invoked") which the name does not convey.

**Suggestion:** Leave as-is. The name is accurate (the legacy path is what runs) and the test is short enough to read. The dual assertion (legacy path runs + SDK branch skipped) is implicit but the test body is 25 lines and reads cleanly. A future test that explicitly asserts "deps.callOpenRouter was not called" would be even more direct, but it is a small improvement on a test that is already correct.

---

### RD-010
| Field    | Value                                              |
|----------|----------------------------------------------------|
| Tier     | 🟡 Follow-up                                        |
| Decide   | Defer                                              |
| Location | `apps/runner/src/lib/opencode.mjs:720-736` — model resolution block |

**Observation:** The model resolution is a three-step chain with the rule spelled out only in the block comment:

```js
// Model resolution order:
//   1. ctx.openrouterModel (env override, used for tests and
//      for the post-QUB-98 cutover when the ConfigMap is gone)
//   2. opencode.json's `model` field, read from the read-only
//      ConfigMap mount. ...
let model = ctx.openrouterModel || "";
if (!model) {
  model = await readOpencodeModel(deps);
}
if (!model) {
  throw new Error(
    "openrouter SDK path: no model configured (set OPENROUTER_MODEL or mount opencode.json)",
  );
}
```

The code expresses "first non-empty value wins, otherwise throw" — the comment is the only place that names the source priorities. After QUB-98 deletes the ConfigMap, the entire `readOpencodeModel` branch dies and the comment is wrong by one step.

**Impact:** The comment is the right kind of "why" (rollout ordering), but it lives above a code block that is *not* the rollout order — it is a fallback chain. A future maintainer who updates this block (e.g., to add a third source) will update the code and may forget the comment. The structural audit (DP-005) suggests adding a one-line log of the source that won; that log would also serve as the docstring for the priority order.

**Suggestion:** Inline the source label into the `deps.log` call at line 738 (the "starting" log). One extra field in the existing log object — `modelSource: model === ctx.openrouterModel ? "env" : "configmap"` — would (a) be greppable for "which source won" in production logs, (b) replace the block comment with a single line at the right point, (c) automatically update when the fallback chain changes. Defer to the first "wrong model on the wrong PR" debug session; not a PR-time change.

---

## Naming Conventions Observed

Derived from the codebase. The Go and Node sides share the kebab-case-file-name convention and prefer `is` / `has` / `should` booleans, but they differ on the case convention for identifiers and on error-shape preference.

### Shared

| Element         | Convention                  | Example                                              |
|-----------------|-----------------------------|------------------------------------------------------|
| File names      | kebab-case + extension      | `opencode.mjs`, `opencode_json.mjs`, `jobbuilder.go` |
| Boolean prefix  | `is` / `has` / `should`     | `hasLabel`, `isAbort`, `skipSkill`                   |
| JSDoc on `export` | above the function       | `openrouter.mjs:24-43` (`callOpenRouter`)            |
| Inline magic value comments | `// 0x1234` or prose at the call site | `opencode.mjs:744` (path: "openrouter-sdk") |

### Go side (`apps/receiver`)

| Element         | Convention                  | Example                                              |
|-----------------|-----------------------------|------------------------------------------------------|
| Variables       | mixedCaps, lower-case initial | `jobName`, `reviewNumber`, `sdkEnabled`              |
| Functions       | mixedCaps, verb + noun      | `submitJob`, `hasLabel`, `resolveSDKEnabled`         |
| Constants (unexported) | lowerCamel + noun     | `sdkEnabledLabel`, `skipReviewLabel`                 |
| Constants (exported)   | PascalCase            | (none added in this PR)                              |
| Errors          | `errors.New(...)` / `fmt.Errorf` | `errors.New("X-GitHub-Installation-ID header missing")` |
| Struct fields   | PascalCase                  | `OpenRouterSDKDefault string`                        |
| Boolean checks  | equality against `"1"`     | `h.cfg.OpenRouterSDKDefault == "1"`                  |
| Doc comments    | above the symbol, complete sentences, name starts with the symbol | `// sdkEnabledLabel is the GitHub label that …` |

### Node side (`apps/runner`)

| Element         | Convention                  | Example                                              |
|-----------------|-----------------------------|------------------------------------------------------|
| Variables       | camelCase                   | `callResult`, `promptBytes`, `killed`                |
| Functions       | camelCase, verb + noun      | `callOpenRouter`, `parseReviewOutput`, `readWithRetry` |
| Constants (module-level) | UPPER_SNAKE_CASE  | `OPENCODE_TIMEOUT_MS`, `LENS_FILES`                  |
| Async functions | `async function` or `export async function` | `runOpenCodeSkill`, `callOpenRouter` |
| Test helpers    | `make`-prefix               | `makeFakeClient`, `makeAssistantResponse`, `makeSdkDeps` |
| Test names      | "subject does thing" prose  | `"callOpenRouter surfaces cached and reasoning tokens when present"` |
| Mock `_`-prefix fields | `_`-prefix for internal-only fields | `_args`, `_logCalls`, `_statusCalls` (some are not asserted) |
| Env-var boolean parsing | `=== "1"`           | `env.BOOP_SKIP_SKILL === "1"`, `env.BOOP_USE_OPENROUTER_SDK === "1"` |
| Error shape     | `new Error("message: <context>")` | `new Error("openrouter SDK path: no model configured (set OPENROUTER_MODEL or mount opencode.json)")` |
| Imports         | ESM named imports, grouped | `import { LENS_FILES, OPENCODE_TIMEOUT_MS } from "./config.mjs";` |

### Cross-cutting

- Both sides use kebab-case file names (`.mjs` for Node, `_test.go` for Go tests, snake_case for Go source files).
- The PR does not introduce any single-letter variable names. The existing convention allows `i` in loops and `e` in errors; this PR adds no new exceptions.
- "Provider" is a real, distinct concept in the SDK path: `provider: "openrouter"` is hard-wired in `buildTelemetry` (`openrouter.mjs:236`) because OpenRouter is the only provider. The "model" field is the parameter; the "provider" field is a label.
- The `skipReviewLabel = "skip-review"` constant on the Go side predates the PR and is the model `sdkEnabledLabel = "boop:openrouter-sdk"` follows (per-PR opt-in via a GitHub label).

---

## Unable to verify

- **Whether the `sdkEnabledLabel` constant's name reads as intended to a first-time reader.** The label `"boop:openrouter-sdk"` carries the namespace prefix (`boop:`), the feature name (`openrouter-sdk`), and reads as a single token. A reader who has not seen the other `skipReviewLabel` pattern may not connect `sdkEnabledLabel` ↔ `"boop:openrouter-sdk"`. Could be intentional — the constant exists so a search-replace for the label string is one line. Worth asking the author whether the name `sdkEnabledLabel` was chosen for short greppability (`grep sdkEnabledLabel`) vs. for cross-file readability.
- **Whether the test names in `openrouter.test.mjs` (e.g. `"buildTelemetry takes the last response (no double-counting across retries)"`) describe the present behavior or the desired post-QUB-95 behavior.** The test asserts that the function returns the input it was handed. The "no double-counting" framing in the name is forward-looking. The intent is correct (the function must not sum), but a reader who only has the test name and the function body has to read the test's comment to confirm the assertion is the right shape. Not flag-worthy; flag for the next change that touches the retry path.
- **Whether `parseReviewOutput("")` followed by `review.parseError = review.parseError || "sdk call failed"` is the right shape, or a relic of an earlier draft.** The `code-quality.md` audit (CQ-003) makes a strong case that the `||` branch is dead. The readability angle: the line is short and reads as "preserve the parser's error, defaulting to 'sdk call failed'" — but the parser always sets one, so the "defaulting" intent is hidden. Could be intentional (defensive against a future parser change) or a leftover. Worth asking the author.
- **Whether the `openrouter.mjs` file-level comment (lines 1-19) belongs at the top of the module or in `docs/development.md`.** The file-level comment is 19 lines and covers the historical context ("the runner used to shell out…"), the current shape, and the telemetry field mapping. The development doc (`docs/development.md`) grew by 94 lines in this PR and likely covers the same ground. If the doc is the canonical narrative, the file comment is duplication; if the file comment is the canonical narrative, the doc is duplication. The skill checklist treats duplication as a readability smell, but I cannot tell which is canonical without reading `docs/development.md` — out of scope for this PR's diff.

---

## Metrics at a Glance

| Item | Count | Notes |
|------|-------|-------|
| Total findings | 10 | 4 🟡 Follow-up, 6 🟢 Optional, 0 🔴 Blocking |
| Findings already covered in other audits | 4 | RD-001 (SP-008), RD-003 (DP-002), RD-004 (CQ-002 / DP-002), RD-010 (DP-005) |
| Findings unique to this readability audit | 6 | RD-002 (string flag surface), RD-005 (`buildTelemetry` signature), RD-006 (`...opts` / `_args` mock smell), RD-007 (comment duplication in `runOpenCodeSkill`), RD-008 (negative-space test name), RD-009 (test name vs. assertion) |
| New function names introduced | 3 | `resolveSDKEnabled` (Go), `callOpenRouter`, `buildTelemetry`, `readOpencodeModel`, `runOpenRouterSkill` (Node) |
| Magic strings introduced | 3 | `"boop:openrouter-sdk"` (Go, 1 site), `"1"` (Go + Node, 5 sites), `"0"` (Go + Node, 3 sites) |
| Magic string constants | 1 | `sdkEnabledLabel` (Go) — `"boop:openrouter-sdk"` |
