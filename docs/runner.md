# boop-runner

One-shot Node.js worker. The half of BoopPr that does the work.

See also: [README](../apps/runner/README.md), [architecture](./architecture.md),
[skills](./skills.md), [output format](./webhook-contract.md#output-format).

## What it does

1. Mints a GitHub App installation token.
2. PATCHes the pre-created status comment to "🤝 paw-shaken in".
3. Clones the PR at `PR_HEAD_SHA` into `/work/repo`.
4. PATCHes the status comment to "🥎 fetched".
5. Builds the boop prompt (orchestrator + 7 lenses inlined).
6. PATCHes the status comment to "👃 sniffing".
7. Calls the OpenRouter SDK in-process with the prompt (25-min hard
   timeout).
8. Parses the `=== SUMMARY === … === INLINE COMMENTS === … === END ===`
   block from the assistant text.
9. PATCHes the status comment to "🦴 bone delivered" (or "❌ lost the bone" on
   failure). On reaction-mode runs (issue_comment triggers, `BOOP_NO_STATUS_COMMENT=1`) the runner does not PATCH or post a status comment; it adds a single terminal reaction (🦴 on done, ❌ on failed) on the trigger comment.
10. Posts the summary as a single PR comment and the inline comments as
    line-pinned review comments.

## File layout

```
apps/runner/
├── src/
│   ├── index.mjs                  # thin orchestrator: loadConfig + run() with injected deps
│   ├── review-header.mjs          # reviewHeader(n) — mirror of Go side
│   ├── review-header.test.mjs     # node:test fixtures
│   └── lib/
│       ├── config.mjs             # loadConfig(env), path constants, STATUS constants
│       ├── log.mjs                # makeLogger(ctx) — JSON logger with pr/sha stamped
│       ├── security.mjs           # assertSafeRef, assertSafeSha, readSecretFile, shortSha
│       ├── git.mjs                # writeNetrc, writeGitconfig, cloneRepo, createCleanupRegistry
│       ├── openrouter.mjs        # callOpenRouter, runOpenCodeSkill, buildBoopPrompt, parseReviewOutput, buildTelemetry, …
│       ├── dashboard.mjs          # postTelemetry, postDashboardStatus — data-layer hooks
│       ├── classify.mjs           # PR classifier stub (QUB-94 sub-workflow)
│       ├── experts.mjs            # multi-expert review (QUB-95), meta-review (QUB-96)
│       ├── rtk.mjs                # rtk adapter (QUB-85) — readFile with rtk + raw fallback
│       ├── tools.mjs              # agent tool set (PR #191): run_command, read_file, git_diff
│       ├── workflow.mjs           # macro + sub-workflow executor
│       └── github.mjs             # mintInstallationToken, postStatus, postReview, postInlineComments, cleanupPriorReview
├── rtk/
│   ├── config.toml                # rtk runtime config (baked into the image)
│   └── filters.toml               # boop custom filter (trusted at build time)
├── package.json                   # @octokit/rest, @openrouter/agent, jsonwebtoken
├── Dockerfile                     # ubuntu 24.04, node 22, rtk 0.44.2, npm ci
└── Makefile                       # install / build / docker
```

`index.mjs` is just the entry point: it loads the config, mints
the token, runs the pipeline, and cleans up. Every side effect
lives in `lib/*.mjs`, each of which accepts a `ctx` (loaded config)
and a `deps` bundle. A test passes fixture `ctx` + stubbed `deps`
to drive any single function without env vars, real network, or
real `git`.

## Dependencies

| Package | Why |
|---|---|
| `@octokit/rest` | GitHub API: post/edit comments, post inline review comments, react. |
| `@openrouter/agent` | In-process OpenRouter Responses API via `callModel`. The runner drives the agent loop, auto-executes a tool set (`run_command`, `read_file`, `git_diff`) for the experts + narrator, and pins a bounded step count + the 25-min wall-clock timer so tool loops cannot eat the Job's 30-min ceiling. The walkthrough stays single-shot (no tools) — only the experts + narrator get the tool set. |
| `jsonwebtoken` | Mint the App JWT for installation-token exchange. |

Node 22 required (matches the `@openrouter/agent` version's test matrix).

## Environment

Required env vars (provided by the Job template, see [receiver.md](./receiver.md#job-template-the-embedded-one)):

| Var | Source | Purpose |
|---|---|---|
| `GITHUB_APP_ID` | `boop-secrets` | App ID for JWT mint. |
| `GITHUB_APP_INSTALLATION_ID` | `boop-secrets` | Installation to exchange the JWT for. |
| `GITHUB_APP_PRIVATE_KEY` | `boop-secrets` | RSA private key (PEM). |
| `PR_OWNER` | Job template | e.g. `qubitquilt` |
| `PR_REPO` | Job template | e.g. `homelab-infra` |
| `PR_NUMBER` | Job template | PR number (string) |
| `PR_HEAD_SHA` | Job template | Head SHA to check out |
| `PR_BASE_REF` | Job template | Base ref (e.g. `main`) for the diff context |
| `PR_PREVIOUS_HEAD_SHA` | Job template | Head SHA of the most recent prior Boop summary; empty on first review. When set, the prompt tells the LLM to diff `PREVIOUS_HEAD_SHA...HEAD_SHA` instead of `BASE...HEAD`. |
| `OPENROUTER_API_KEY` | `boop-secrets` | LLM API key |
| `OPENROUTER_MODEL` | Job template | Model id (e.g. `minimax/minimax-m3`). The SDK is the only invocation path; this is where the model name comes from. |
| `BOOP_STATUS_COMMENT_ID` | Job template | Status comment to PATCH (empty if none; runner falls back to posting fresh) |
| `BOOP_REACTION_COMMENT_ID` | Job template | Comment to react on failure (usually the trigger comment) |
| `BOOP_REVIEW_NUMBER` | Job template | 1-based index of this review; used for the `re-review #N` header |
| `BOOP_BOT_LOGIN` | `BOT_LOGIN` on receiver | GitHub login of the bot App (e.g. `booppr[bot]`). When set on a re-review, the runner resolves outdated Boop review threads and minimizes prior Boop issue comments. |
| `BOOP_SKIP_SKILL` | Job template | `1` for a minimal-prompt smoke test (debug only) |
| `BOOP_RTK_DISABLED` | Job template | `1` to bypass the [rtk adapter](#rtk-adapter-qub-85) — the runner's file reads go straight to `fs.readFile`, the pre-QUB-85 behavior. Use to reproduce a regression or to debug a poisoned rtk install without rebuilding the image. |
| `BOOP_NO_STATUS_COMMENT` | Job template | `1` to skip the status comment surface and use reactions on the trigger comment instead (QUB-114). The receiver sets this on issue_comment triggers; the runner does NOT post or PATCH a status comment, and adds a single terminal reaction (🦴 on done, ❌ on failed) on the trigger comment. |

The runner exits 1 if any required env var is missing. The Job template
sets all of them; if a value is missing the Job fails to start.

## Lifecycle (`main`)

```
1.  Parse env, validate required vars
2.  Mint installation token                       ← auth
3.  PATCH status → "🤝 paw-shaken in"            ← status: auth
4.  git clone --depth 50, fetch --depth 200, checkout HEAD_SHA
                                                   ← clone
5.  PATCH status → "🥎 fetched"                    ← status: clone
6.  Build the boop prompt (orchestrator + 7 lenses inlined from the
    ConfigMap mount at /home/opencode/.config/opencode)
7.  PATCH status → "👃 sniffing"                   ← status: review
8.  Call the OpenRouter SDK in-process (no subprocess, no PTY wrap)
     ↳ hard-kill at 25 min
9.  Parse the assistant text for the structured block
10. Post summary as PR comment                    ← done
11. Post each inline comment (best-effort)
12. PATCH status → "🦴 bone delivered"           ← status: done
```

Each `postStatus(stage, detail)` PATCHes the existing status comment
additively: a `<!-- boop-timeline -->` separator splits the receiver's
header from the runner's timeline. Earlier entries are trimmed if the
combined body exceeds 60 KB.

**QUB-114 — reaction mode on issue_comment triggers.** When the
review is triggered by an issue comment (the user wrote
`@BoopPr review` on a PR), the receiver sets
`BOOP_NO_STATUS_COMMENT=1` and the runner skips the entire
status-comment path. The author already saw the 👀 reaction
the receiver added; the runner does NOT create or PATCH a
status comment, and no interim PATCHes happen. The runner
adds a single terminal reaction to the trigger comment
via `postFinalReaction`:

- Done → 🦴 (bone) reaction
- Failed → ❌ (cross) reaction

The author's view on a comment-triggered re-review is
`👀` → `🦴` (or `❌`). One reaction change, one
notification, no PATCH loop.

On any error, `postStatus("failed", err.message)` runs and the runner
exits 1. The Job's `activeDeadlineSeconds=1800` is the wall-clock
ceiling; the runner's 25-min SDK timeout is a tighter inner ceiling.

## GitHub App auth

In-process (no `go-github` equivalent; we use `jsonwebtoken` + `fetch`):

1. `jwt.sign({ iat: now-30, exp: now+600, iss: appId }, privateKey, { algorithm: 'RS256' })`
   — 10-min App JWT.
2. `POST https://api.github.com/app/installations/{installationId}/access_tokens`
   with `Authorization: Bearer <jwt>` → installation token (1h TTL).
3. Octokit client uses the installation token for all subsequent calls.

No persistence; the token lives for the lifetime of the pod.

## Clone

```js
git clone --depth 50 https://x-access-token:${token}@github.com/${owner}/${repo}.git /work/repo
git fetch --depth 200 origin ${baseRef} ${headSHA}
git checkout ${headSHA}
```

Shallow clone (50 commits), then a deep-enough fetch of the base ref and
head SHA so the diff between them is reachable. The checkout puts the
working tree at the PR head. Each step has a 5-min timeout.

## OpenRouter agent invocation

The runner calls `@openrouter/agent` in-process via `client.callModel`.
The agent SDK runs on top of the OpenResponses API and gives the
runner three things the pre-swap `chatSend` path lacked:

1. **Tool auto-execution.** The runner hands the agent a tool set
   (`run_command`, `read_file`, `git_diff` — see `lib/tools.mjs`)
   and the SDK loops the model through tool calls until the model
   produces a text response or the step budget runs out. The
   walkthrough stays single-shot (no tools); the experts and
   narrator opt in.
2. **Bounded step count.** `callModel({ ..., stopWhen:
   stepCountIs(STEP_CAP) })` with `STEP_CAP = 10` keeps a runaway
   agent from eating the 25-min wall-clock budget; the timer still
   races the call as a hard kill.
3. **Structured response access.** `result.getText()` returns the
   final text after tool execution; `result.getResponse()` returns
   the OpenResponses response (`{ id, model, output, usage }`) so
   `extractUsage` can map the camelCase `Usage` shape into the
   runner's snake_case telemetry contract.

The model comes from `OPENROUTER_MODEL`; the API key is loaded
from the mounted `boop-secrets` file at startup and passed via
`env.OPENROUTER_API_KEY`. The 25-min hard-kill timer races the
SDK call; on timeout, `AbortController.abort()` surfaces an
`AbortError` and the runner treats it as a clean failure
(`postStatus("failed", "openrouter run exceeded 25-min timeout")`).

The SDK response carries the assistant text and the usage block
(`inputTokens`, `outputTokens`, `totalTokens`, `cost`, optional
`cachedTokens` and `reasoningTokens`). `buildTelemetry` rolls the
SDK response into the runner's telemetry shape and the dashboard
endpoint POSTs it on the runner's last status update.

No subprocess. No PTY wrap. The tool execute functions run in the
runner process (uid 1000 in the K8s Job); the `run_command`
guard denies network primitives and secret-mount references, and
strips the runner's process env from the spawned shell (PATH +
HOME + NODE_ENV only). The prompt's instruction hierarchy is the
primary control against a prompt-injected LLM trying to exfil;
the guard is the belt.

The runner's prompt is the assembled boop message; the agent
loops tool calls until it produces the final `=== SUMMARY === … ===
END ===` block or the step budget runs out.

## Prompt construction (`buildBoopPrompt`)

The runner uses a six-stage pipeline (QUB-95 + multi-expert):
walkthrough → pick experts → dispatch → gather → meta-review
→ narrate. Three LLM prompt templates are in play:

1. **Walkthrough** — `lib/walkthrough.mjs:buildWalkthroughPrompt`.
   One LLM call reads the diff and produces a 10–20 sentence
   human-readable summary. The expert sub-agents consume it
   as shared context.
2. **Expert dispatch** — `lib/experts.mjs:buildExpertPrompt`.
   Each expert is one LLM call. The system prompt is the
   expert's lens file (`agents/review-<expert>.md`). The user
   message is the walkthrough + the diff + the PR context.
   The expert returns JSON: `{ findings: Finding[] }`.
3. **Narrator** — `lib/openrouter.mjs:buildBoopPrompt`. The
   final LLM call synthesizes the walkthrough + the gathered
   expert findings into the structured block. The
   narrator does not walk the lenses (the experts did).

The orchestrator (`skills/boop/SKILL.md`) is read from the
ConfigMap mount at `/home/opencode/.config/opencode` and
inlined into the narrator's prompt. The lens files are
read into the per-expert prompts (one per call) — they
are not inlined into the narrator's prompt. The
`..data -> ..2026_…` symlink can be transiently
inconsistent right after pod start, so each read retries
with a 1s linear backoff (5 attempts).

Frontmatter (`---\n…\n---\n`) is stripped from each file
before inlining so the model sees clean prompt content.

The reads go through the [rtk adapter](#rtk-adapter-qub-85) (QUB-85).
The adapter shells out to `rtk read` for compression; when rtk is
disabled or the binary is missing the reads fall back to raw
`fs.readFile`. The adapter is transparent to the rest of the runner.

The narrator's prompt (the only one the orchestrator
runner module builds) has the structure:

1. Role: "You are the narrator for the BoopPr GitHub App's
   multi-expert review. ..."
2. Output format spec (the `=== SUMMARY ===` / `=== INLINE COMMENTS ===`
   / `=== END ===` block — see
   [output format](./webhook-contract.md#output-format)).
3. Output rules: 3-8 inline comments, line numbers refer to file *after*
   diff, only on added/modified lines, no empty sections.
4. Orchestrator (boop skill body, frontmatter stripped). Describes
   the narrator's role: synthesize, do not re-walk.
5. Walkthrough (human-readable summary of the PR — see below).
6. Expert findings (the source material to synthesize).
7. PR context (owner/repo, PR number, head SHA, base ref, working dir).

## Output parsing (`parseReviewOutput`)

The model is required to emit exactly:

```
=== SUMMARY ===
<markdown>
=== INLINE COMMENTS ===
path/to/file.ext:LINE: <comment body>
path/to/other.ext:LINE: <comment body>
=== END ===
```

The parser:

1. Runs a regex that captures the SUMMARY body and the INLINE COMMENTS
   body. The regex is case-insensitive and tolerates whitespace.
2. Runs a structure sanity check on the SUMMARY body. A real review
   is at least 200 bytes, contains a markdown heading or a finding
   table, and does not look like a JS string-concat echo, a fake
   shell transcript, or a raw error string. Failures return an
   empty summary + a `parseError` reason; the runner skips the post
   and surfaces the reason in the status thread.
3. For each line in the INLINE COMMENTS block, matches
   `^(\S+?):(\d+):\s+(.*)$`. Skips lines that don't match. Builds
   `{ path, line, body }` records.

The runner then:

- Posts the summary as a single `POST /repos/{owner}/{repo}/issues/{n}/comments`
  with the `## 🐾 Boop's review` (or `re-review #N`) header, the body,
  and the standard footer.
- Posts each inline as `POST /repos/{owner}/{repo}/pulls/{n}/comments`
  with `commit_id`, `path`, `line`, `side: "RIGHT"`. Each inline is
  independent — a single failure does not block the rest.

The summary body is trimmed to 65 KB (GitHub's hard cap on issue
comments) by replacing the tail with `…(truncated)`.

## Status thread (`postStatus`)

The receiver pre-creates the status comment with a header like:

> 🐾 **Boop's on the case!** (review)
>
> Triggered by @alice
>
> Last commit: `a1b2c3d`. Sniffing now — updates will appear here.
>
> <!-- boop-timeline -->

The runner reads the comment, finds the `<!-- boop-timeline -->`
separator, and appends one line per stage. The header (label, trigger,
commit) is **not** modified.

If the combined body exceeds 60 KB, the older timeline is trimmed and
the new entry is appended.

Status stages and emojis (must match the receiver's vocabulary):

| Stage | Emoji | Short label | Body |
|---|---|---|---|
| (initial) | 🐾 | `🐾 Boop's on the case!` | Initial header created by the receiver |
| `auth` | 🤝 | `🤝 paw-shaken in` | "Paw-shaken in — authenticated with GitHub at `<sha>`." |
| `clone` | 🥎 | `🥎 fetched` | "Boop fetched the repo at `<sha>`. Checking out the PR head and starting the multi-lens review." |
| `review` | 👃 | `👃 sniffing` | "Boop is sniffing — running the multi-lens review on `<sha>`." |
| `done` | 🦴 | `🦴 bone delivered` | "Boop brought you a bone. See the comment below." |
| `failed` | ❌ | `❌ lost the bone` | "Boop lost the bone. Check the Job logs for details." (with a `<details>` block carrying the error tail) |

## Review header

`src/review-header.mjs` exports `reviewHeader(n)`. Must match
`github.ReviewSummaryHeader(n)` in `apps/receiver/internal/github/client.go`.

```
reviewHeader(1)   === "## 🐾 Boop's review"
reviewHeader(2)   === "## 🐾 Boop's re-review #2"
reviewHeader(10)  === "## 🐾 Boop's re-review #10"
```

`n <= 1` and falsy inputs collapse to the first-review header. Tests on
both sides pin this.

## Tests

```
cd apps/runner
make build                              # node --check src/index.mjs
make test                               # bun test src/*.test.mjs src/lib/*.test.mjs
```

`make test` runs the local test loop under Bun. The production
runner image still uses Node 22; this is a local-iteration
acceleration. `make test-node` falls back to `node --test` for
the rare case Bun disagrees with node on a test. See
[QUB-10](https://linear.app/qubit-quilt/issue/QUB-10/convert-runner-to-using-bun).

Tests are granular — one file per module under `src/lib/`:

- `src/lib/config.test.mjs` — `loadConfig` (env → ctx, defaults, error cases).
- `src/lib/log.test.mjs` — `makeLogger` shape and JSON stamping.
- `src/lib/security.test.mjs` — `assertSafeRef`, `assertSafeSha`, `shortSha`, `readSecretFile`.
- `src/lib/git.test.mjs` — `createCleanupRegistry` (parallel + idempotent) and `cloneRepo` (with mock fs + execFile; verifies each git argv, env, and the netrc/gitconfig content).
- `src/lib/openrouter.test.mjs` — `callOpenRouter` (fake `callModel` returning a ModelResult-shaped object, success / 4xx / abort / no text / token mapping for both OpenResponses `inputTokens`/`outputTokens` and the legacy ChatUsage `promptTokens`/`completionTokens` shapes), `buildTelemetry` (success / failure stamp), `parseReviewOutput` (structure sanity check + the five 2026-08-03 failure shapes), `buildBoopPrompt` (mock fs; verifies H5 markers, lens ordering, frontmatter stripping, re-review vs first-review diff range, `stripOpenRouterPrefix`, the QUB-85 rtk-adapter path, and the QUB-<next tools-enabled / tools-disabled "What you are receiving" variants), `runOpenCodeSkill` (agent branch happy path, SDK failure, AbortError, toolCount log line), and the `extractAssistantText` legacy chat-completion fallback.
- `src/lib/rtk.test.mjs` — `createRtkAdapter` (QUB-85: BOOP_RTK_DISABLED bypass, missing-binary fallback, rtk CLI shape, per-call overrides, rtk-failure raw fallback, init memoisation, source getter, single-fallback-log, custom binary name).
- `src/lib/github.test.mjs` — `mintInstallationToken`, `postStatus`, `postReview`, `postInlineComments` (parallel + partial failures), `cleanupPriorReview` (parallel fetches + pagination + error counting).

`src/index.test.mjs` is the integration test: it drives `run(env, overrides)` end-to-end with every side effect stubbed — fetch returns canned responses, Octokit is a recording fake, `spawn` and `execFile` are stubs, `runOpenCodeSkill` returns a canned review — and asserts the orchestration order (auth → review → done), failure paths, re-review cleanup gating, and defense-in-depth gates.

The `review-header.test.mjs` mirrors
`apps/receiver/internal/github/review_header_test.go` — change one, change both.

## rtk adapter (QUB-85)

The runner's file reads (today: the SKILL.md and the seven lens files
from the ConfigMap mount) go through the rtk adapter in
`src/lib/rtk.mjs`. The adapter is the single place the runner shells
out to `rtk read`; it routes reads through rtk when the binary is
present and falls back to raw `fs.readFile` when rtk is missing or
disabled. The runner-side kill switch is `BOOP_RTK_DISABLED=1`; the
rtk-side kill switch is `RTK_DISABLED=1` on the per-call execFile
env (also wired). Both are belt-and-suspenders: a future config edit
that re-enables rtk on one side does not bypass the other.

The image ships the rtk binary at `/usr/local/bin/rtk`, the
`config.toml` at `/home/opencode/.config/rtk/config.toml`, and the
custom `filters.toml` at `/home/opencode/.config/rtk/filters.toml`.
The custom filter (`boop-review-read`) caps the lens file reads at
400 lines and 500 chars/line so a runaway file cannot blow up the
prompt context. The trust store at
`/home/opencode/.local/share/rtk/trusted_filters.json` is populated
at image build (`rtk trust --yes`) so the filter is on the trust
list the moment the pod starts — no interactive prompt, no first-run
gap.

Telemetry is killed at three layers: the config's
`[telemetry] enabled = false`, the baked env
`RTK_TELEMETRY_DISABLED=1`, and the per-call
`RTK_TELEMETRY_DISABLED=1` the adapter forwards. Tee lands at
`/work/rtk-tee` (an `RTK_TEE_DIR` env var) in `failures` mode so a
crashed rtk call's recovery hint stays in the same namespace the pod
owns.

The adapter's resolved mode (`rtk` or `raw`, plus the resolved
binary path or fallback reason) is logged on startup so an
operator tailing the pod can confirm rtk is in the expected path
without waiting for the first file read.

## Build

```
make docker-build IMAGE=ghcr.io/qubitquilt/boop-runner:dev
make docker-push
```

The Dockerfile supports `linux/amd64` and `linux/arm64` (QUB-85
acceptance). The current `boop-runner-set` Actions runner is arm64
and the cluster nodes are arm64, so the workflow pins
`platforms: linux/arm64`. The Dockerfile's rtk install RUN
selects the right prebuilt tarball from the GitHub release per
`TARGETARCH` and verifies a pinned SHA-256. To flip the workflow
to multi-arch, set `platforms: linux/amd64,linux/arm64` and the
existing per-arch buildx invocation will exercise the second
branch.

`npm install` runs `os=linux --cpu=arm64` so npm pulls the right native
binaries for the platform (the `@openrouter/sdk` postinstall has a
`scripts/check-types.js` step that runs in the build environment).

The image is run as the `ubuntu` user (uid 1000) so `/work` and the
home directory are writable for the runner. The ConfigMap mount
target (`/home/opencode/.config/opencode`) is created at build time
so the kubelet can mount there; the chown step intentionally
excludes that path because the kubelet overlays it at runtime.

## Failure modes

- **Env var missing.** Throws at startup, Job exits 1. The error is
  logged JSON; the status comment is never created (the receiver
  creates it pre-Job). The receiver's pre-created status comment stays
  stuck at the initial "🐾 Boop's on the case!" stage. To recover, re-trigger the
  webhook (push a commit) so the receiver submits a fresh Job.
- **Clone fails.** `cloneRepo` throws; `main` catches, `postStatus("failed")`,
  rethrows, Job exits 1.
- **OpenRouter SDK times out (25 min).** AbortController fires,
  `main` throws, `postStatus("failed", "openrouter run exceeded
  25-min timeout")`, Job exits 1.
- **OpenRouter SDK returns 4xx / 5xx.** `main` returns an empty
  review with the SDK error stamped on the telemetry
  (`telemetry.error`). The status gate rejects the empty summary
  with the SDK error in the reason; the run ends as `failed`.
- **OpenRouter SDK returns no assistant text.** `main` returns an
  empty review; the status gate rejects with `summary parse
  failed: no structured block`; the run ends as `failed`.
- **Summary PATCH fails.** Non-fatal; logged, the next stage PATCH
  continues. The status thread may have a gap.
- **Inline comment post fails.** Non-fatal; each one is independent and
  the rest still post. Logged per failure.

## Debugging a stuck run

```
kubectl get jobs -n dev-tools -l app=boop,pr-number=<N>
kubectl logs -n dev-tools -l app=boop,pr-number=<N> --tail=200
kubectl describe job -n dev-tools boop-<owner>-<repo>-<N>-<sha7>
```

The runner logs JSON to stdout. Stages: `start`, `auth`, `clone`,
`skill`, `opencode` (the SDK call — log tag kept stable from the
pre-SDK era so dashboard log queries that filter by
`stage:"opencode"` survive the cutover), `status`, `review`,
`inline`, `done`, `fatal`.

## See also

- [skills.md](./skills.md) — the boop skill and its seven lenses.
- [webhook-contract.md](./webhook-contract.md#output-format) — the
  `=== SUMMARY ===` / `=== INLINE COMMENTS ===` / `=== END ===` block
  the runner parses.
- [architecture.md](./architecture.md) — system-level flow.
