# boop-runner

One-shot Node.js worker. The half of BoopPr that does the work.

See also: [README](../apps/runner/README.md), [architecture](./architecture.md),
[skills](./skills.md), [output format](./webhook-contract.md#output-format).

## What it does

1. Mints a GitHub App installation token.
2. PATCHes the pre-created status comment to "🔐 authenticated".
3. Clones the PR at `PR_HEAD_SHA` into `/work/repo`.
4. PATCHes the status comment to "📥 fetched".
5. Builds the boop prompt (orchestrator + 7 lenses inlined).
6. PATCHes the status comment to "🧠 reviewing".
7. Runs `opencode run` against the repo with the prompt (25-min hard
   timeout).
8. Parses the `=== SUMMARY === … === INLINE COMMENTS === … === END ===`
   block from stdout.
9. PATCHes the status comment to "✅ review in" (or "❌ distracted" on
   failure).
10. Posts the summary as a single PR comment and the inline comments as
    line-pinned review comments.

## File layout

```
apps/runner/
├── src/
│   ├── index.mjs                  # main: orchestration, status updates, posting
│   ├── review-header.mjs          # reviewHeader(n) — mirror of Go side
│   └── review-header.test.mjs     # node:test fixtures
├── package.json                   # opencode-ai, @octokit/rest, jsonwebtoken
├── Dockerfile                     # ubuntu 24.04, node 22, opencode-ai npm
└── Makefile                       # install / build / docker
```

## Dependencies

| Package | Why |
|---|---|
| `@octokit/rest` | GitHub API: post/edit comments, post inline review comments, react. |
| `jsonwebtoken` | Mint the App JWT for installation-token exchange. |
| `opencode-ai` | The LLM CLI. Installed as a node_module; symlinked to `/usr/local/bin/opencode` so the runner can shell out to `opencode run`. |

Node 22 required (matches the opencode-ai version's test matrix).

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
| `BOOP_STATUS_COMMENT_ID` | Job template | Status comment to PATCH (empty if none; runner falls back to posting fresh) |
| `BOOP_REACTION_COMMENT_ID` | Job template | Comment to react on failure (usually the trigger comment) |
| `BOOP_REVIEW_NUMBER` | Job template | 1-based index of this review; used for the `re-review #N` header |
| `BOOP_SKIP_SKILL` | Job template | `1` for a minimal-prompt smoke test (debug only) |

The runner exits 1 if any required env var is missing. The Job template
sets all of them; if a value is missing the Job fails to start.

## Lifecycle (`main`)

```
1.  Parse env, validate required vars
2.  Mint installation token                       ← auth
3.  PATCH status → "🔐 authenticated"            ← status: auth
4.  git clone --depth 50, fetch --depth 200, checkout HEAD_SHA
                                                   ← clone
5.  PATCH status → "📥 fetched"                   ← status: clone
6.  Materialize config (cp -r the ConfigMap mount into a writable dir)
7.  Build the boop prompt                         ← review prep
8.  PATCH status → "🧠 reviewing"                 ← status: review
9.  spawn `script -qfc 'opencode run …'` (PTY wrapper)
    ↳ hard-kill at 25 min
10. Strip ANSI, parse review output
11. Post summary as PR comment                    ← done
12. Post each inline comment (best-effort)
13. PATCH status → "✅ review in"                 ← status: done
```

Each `postStatus(stage, detail)` PATCHes the existing status comment
additively: a `<!-- boop-timeline -->` separator splits the receiver's
header from the runner's timeline. Earlier entries are trimmed if the
combined body exceeds 60 KB.

On any error, `postStatus("failed", err.message)` runs and the runner
exits 1. The Job's `activeDeadlineSeconds=1800` is the wall-clock
ceiling; the runner's 25-min opencode timeout is a tighter inner ceiling.

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

## OpenCode invocation

The `opencode run` binary is a TUI. In a K8s pod there is no controlling
terminal, which makes the binary hang at init. Workaround: wrap the call
in `script -qfc …` to allocate a PTY. Combined with `--auto`, the binary
boots headless and writes the assistant response to the pty master
(which Node reads as stdout).

```js
script -qfc 'opencode run "<repo-dir>" "<prompt>" --auto' /dev/null
```

Env on the child:

- `HOME=/tmp/opencode-home`, `XDG_CONFIG_HOME=/tmp/opencode-config` — the
  `ConfigMap` mount is read-only; opencode needs a writable home.
- `OPENCODE_CONFIG_CONTENT=<json>` — the resolved opencode.json (model,
  provider, baseURL).
- `OPENCODE_CONFIG_DIR=/tmp/opencode-config/opencode` — the materialized
  copy.
- `TERM=xterm-256color` — sane default if `TERM` is unset.

`OPENCODE_DEBUG=1` adds `--log-level DEBUG --print-logs` for verbose
diagnostics.

The runner kills the subprocess with `SIGKILL` after 25 min, regardless
of state.

## Prompt construction (`buildBoopPrompt`)

The orchestrator (`skills/boop/SKILL.md`) and each of the seven lenses
(`skills/boop/agents/review-*.md`) are read from the ConfigMap mount at
`/home/opencode/.config/opencode` and inlined into the prompt. The
runner reads **directly from the source mount**, not from the writable
copy — `cp -rL` on the `..data` symlink can pull all previous ConfigMap
versions into the destination and OOM the container.

Lens files are read with retry (1s backoff × 5 attempts) to absorb the
`..data -> ..2026_…` symlink race right after pod start.

Frontmatter (`---\n…\n---\n`) is stripped from each file before inlining
so the model sees clean prompt content.

The final prompt has the structure:

1. Role: "You are running inside a Kubernetes Job triggered by a GitHub
   App. Review the pull request …"
2. Output format spec (the `=== SUMMARY ===` / `=== INLINE COMMENTS ===`
   / `=== END ===` block — see
   [output format](./webhook-contract.md#output-format)).
3. Output rules: 3-8 inline comments, line numbers refer to file *after*
   diff, only on added/modified lines, no empty sections.
4. Orchestrator (boop skill body, frontmatter stripped).
5. Each lens as a labeled `### Lens: <name>` block.
6. PR context (owner/repo, PR number, head SHA, base ref, working dir).

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

1. Strips ANSI escape sequences (`\x1b[...m`, `\x1b[...A/B/C…`, OSC
   sequences).
2. Runs a regex that captures the SUMMARY body and the INLINE COMMENTS
   body. The regex is case-insensitive and tolerates whitespace.
3. If the structured block is missing, falls back to the whole stdout as
   the summary (so a malformed model output still produces *something*).
4. For each line in the INLINE COMMENTS block, matches
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

> 👀 **boop is reviewing this PR...** (review)
>
> Triggered by @alice
>
> Last commit: `a1b2c3d`. Updates will appear here.
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
| `auth` | 🔐 | `🔐 authenticated` | "Boop has arrived — authenticated with GitHub at `<sha>`." |
| `clone` | 📥 | `📥 fetched` | "Boop fetched the repo at `<sha>`. Checking out the PR head and starting the multi-lens review." |
| `review` | 🧠 | `🧠 reviewing` | "Boop is reviewing — running the multi-lens review on `<sha>`." |
| `done` | ✅ | `✅ review in` | "Boop's review is in. See the comment below." |
| `failed` | ❌ | `❌ distracted` | "Boop got distracted. Check the Job logs for details." (with a `<details>` block carrying the error tail) |

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
make build                  # node --check src/index.mjs
node --test src/*.test.mjs  # runs review-header.test.mjs
```

The test file lives next to the source (Node 22 supports
`node --test src/*.test.mjs`). The header test mirrors
`apps/receiver/internal/github/review_header_test.go` — change one, change
both.

## Build

```
make docker-build IMAGE=ghcr.io/qubitquilt/boop-runner:dev
make docker-push
```

The Dockerfile is `linux/arm64` only (the `boop-runner-set` Actions
runner is arm64; the cluster nodes are arm64). Multi-arch would require
a QEMU buildx setup; not currently configured.

`npm install` runs `os=linux --cpu=arm64` so npm pulls the right native
binaries (opencode-ai ships a precompiled binary).

The image is run as the `ubuntu` user (uid 1000) so opencode-ai's
postinstall has a writable HOME.

## Failure modes

- **Env var missing.** Throws at startup, Job exits 1. The error is
  logged JSON; the status comment is never created (the receiver
  creates it pre-Job). The receiver's pre-created status comment stays
  stuck at the initial "👀 reviewing" stage. To recover, re-trigger the
  webhook (push a commit) so the receiver submits a fresh Job.
- **Clone fails.** `cloneRepo` throws; `main` catches, `postStatus("failed")`,
  rethrows, Job exits 1.
- **OpenCode times out (25 min).** Killed with SIGKILL. `main` throws,
  `postStatus("failed", "opencode run exceeded 25-min timeout")`, Job
  exits 1.
- **OpenCode exits non-zero.** `main` throws with the last 30 lines of
  stderr in the message. Status is `failed` with that detail.
- **OpenCode returns empty stdout.** `main` throws, `postStatus("failed")`,
  Job exits 1.
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
`skill`, `opencode`, `status`, `review`, `inline`, `done`, `fatal`.

## See also

- [skills.md](./skills.md) — the boop skill and its seven lenses.
- [webhook-contract.md](./webhook-contract.md#output-format) — the
  `=== SUMMARY ===` / `=== INLINE COMMENTS ===` / `=== END ===` block
  the runner parses.
- [architecture.md](./architecture.md) — system-level flow.
