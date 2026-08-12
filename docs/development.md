# Development

Local dev workflow for the receiver, runner, and skill edits.

See also: [receiver.md](./receiver.md), [runner.md](./runner.md),
[deployment.md](./deployment.md), [secrets.md](./secrets.md).

## Prereqs

- Go 1.23 (`go version`).
- Node 22 (`node --version`).
- Bun (for the runner's local test loop — `bun --version`). The
  production runner image still runs Node; the local test loop runs
  under Bun for fast iteration. See [QUB-10](https://linear.app/qubit-quilt/issue/QUB-10/convert-runner-to-using-bun).
- [`rtk`](https://github.com/rtk-ai/rtk) on `PATH` (QUB-85). The
  runner's file reads go through the [rtk adapter](./runner.md#rtk-adapter-qub-85)
  when the binary is present; a missing binary falls back to raw
  `fs.readFile` automatically. `brew install rtk` on macOS, `apt
  install rtk` (or download from the GitHub release) on Linux.
- A kubeconfig with permissions to submit Jobs in a `dev-tools`
  namespace (for receiver end-to-end).
- A GitHub App's `app-id`, `installation-id`, and `private-key` (for any
  path that calls the GitHub API).
- `kubectl` and (optionally) `smee` for webhook forwarding.

## Receiver (`apps/receiver/`)

### Build

```
cd apps/receiver
make build          # → ./bin/receiver
make test           # go test ./...
make vet            # go vet ./...
```

### Run locally (no cluster)

For unit testing the HTTP layer without a real cluster, the
`internal/webhook` package is testable in isolation; the tests don't
require an in-cluster client.

### Run locally (with a cluster)

```
export WEBHOOK_SECRET=$(openssl rand -hex 32)
export GITHUB_APP_ID=...
export GITHUB_APP_PRIVATE_KEY="$(cat /path/to/key.pem)"
export KUBECONFIG=~/.kube/config
make run            # ./bin/receiver on :8080
```

Smoke test:

```
curl -sS -X POST -H 'X-GitHub-Event: ping' http://localhost:8080/webhook
# expect: 202 with status: ignored
```

For a real PR event, use smee.io to forward GitHub webhooks to your
local receiver.

### Container

```
make docker-build IMAGE=ghcr.io/<your-handle>/boop-receiver:dev
make docker-push
```

## Runner (`apps/runner/`)

### Build

```
cd apps/runner
make install        # npm ci
make typecheck      # tsc --noEmit
make build          # tsc (writes dist/index.js)
make test           # bun test src/*.test.ts src/lib/*.test.ts
make test-node      # node --test --experimental-strip-types
```

QUB-136: the runner is TypeScript. `make build` compiles `src/`
into `dist/`; the runtime image runs `node dist/index.js`, no
`tsx` in production. `make test` runs under Bun (the hot path).
`make test-node` runs the same suite under
`node --test --experimental-strip-types` (parity check — node
strips the type annotations and runs the same source). Both
must pass before the build-runner GitHub Action will land a
new :stable digest.
See [QUB-10](https://linear.app/qubit-quilt/issue/QUB-10/convert-runner-to-using-bun)
for the rationale.

### Run locally (no cluster, no LLM)

For the header format and other pure helpers, the test suite is the only check:

```
cd apps/runner
make test                                              # all tests, under Bun
bun test src/review-header.test.ts                      # just the header fixtures
bun test src/lib/github.test.ts                         # GitHub API surface
node --test --experimental-strip-types src/lib/openrouter.test.ts  # SDK call, prompt builder, parser
```

### Run locally (against a real PR, outside a Job)

The runner is designed to run inside a K8s Job; all its env vars
(PR_OWNER, PR_REPO, PR_NUMBER, PR_HEAD_SHA, PR_BASE_REF, GITHUB_APP_*,
OPENROUTER_API_KEY, BOOP_*) are set by the Job template. To run it
locally, copy `.env.local.example` to `.env.local`, fill in the
secrets, and use the `local-run` wrapper which does the env-var
sanity check, the OpenRouter-key → secret-path bootstrap, and
the skill-mount + pre-staged-repo verification before invoking
the runner:

```
cd apps/runner
cp .env.local.example .env.local
# edit .env.local: set OPENROUTER_API_KEY, point
# BOOP_GITHUB_APP_PRIVATE_KEY_PATH at the PEM you downloaded
# from the App's settings page, set PR_HEAD_SHA to the commit
# you want reviewed
(source .env.local && make local-run)
# equivalent to: (source .env.local && npm run local-run)
```

The wrapper stubs `cloneRepo` so it doesn't try to re-clone over
the pre-staged repo (the production flow uses the App's installation
token for the clone, which doesn't work for a workstation clone
via `gh repo clone`). Everything else is real: the narrator calls
the OpenRouter SDK, the GitHub side effects PATCH the status
comment + POST the summary + inlines against the actual PR. A
local run is a real review of a real PR.

QUB-136: the wrapper runs via `bun scripts/local-run.ts` so the
local dev loop does not need a `tsc` build first. Production
goes through `tsc → dist/index.js`; the dev loop bypasses the
build for iteration speed. The local dev loop runs under
`bun` end-to-end (`bun install`, `bun x tsc`, `bun test`); the
only `node` invocation is `make test-node` for the production-
runtime parity check.

The runner will:

1. Mint a token.
2. Skip status updates (BOOP_STATUS_COMMENT_ID is empty, the runner
   logs `skip (no client or comment id)`).
3. Clone the PR.
4. Run the boop skill.
5. Post a fresh `## 🐾 Boop's review` comment.
6. Post inline comments.
7. Exit 0.

The PR you point it at will get a review comment. **Use a test PR.**

### Container

```
make docker-build IMAGE=ghcr.io/<your-handle>/boop-runner:dev
make docker-push
```

The build is `linux/arm64` only. On a non-arm64 host, use
`docker buildx` with a cross-platform builder (the CI workflow shows
the buildx setup).

## Skill edits (`apps/k8s/base/runner-config/skills/boop/`)

No image rebuild. Edit the file, commit, push. ArgoCD syncs the
ConfigMap; the next PR review uses the new version.

To preview a skill edit locally without ArgoCD:

```
# 1. Edit SKILL.md or any agents/review-*.md.
# 2. Render the ConfigMap from the current kustomize base:
kubectl -n dev-tools create configmap boop-runner-config \
  --from-file=apps/k8s/base/runner-config/ \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. The next Job submits and picks up the new ConfigMap.
```

For a faster preview, render the edited skill files into a
local ConfigMap and point the runner at it:

```
# 1. Edit SKILL.md or any agents/review-*.md.
# 2. Stage the edits as a ConfigMap and let the runner pick them up:
kubectl -n dev-tools create configmap boop-runner-config \
  --from-file=apps/k8s/base/runner-config/ \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. The next Job submits and picks up the new ConfigMap.
```

## Image publishing (CI)

The CI workflow runs on push to `main`, on `v*` tags, and on
`pull_request` to `main`. It is the same workflow for both images:

- `build-receiver.yaml`
- `build-runner.yaml`

To trigger a new image build without a tag push:

```
git tag v0.2.0
git push origin v0.2.0
```

This pushes `stable`, `v0.2.0`, `0.2`, `0`. The default overlay pins
`latest` (main HEAD); override to a specific tag for reproducible
rollouts.

## OpenRouter agent invocation (post PR #191)

The runner calls
[`@openrouter/agent`](https://github.com/openrouterteam/typescript-sdk)
in-process via `client.callModel`. There is no subprocess, no
`script(1)` PTY wrap, and no `opencode.json` template. The model
name comes from the `OPENROUTER_MODEL` Job env var; the API key
comes from the `boop-secrets` mounted file. Telemetry is captured
from the SDK
response directly (`prompt_tokens`, `completion_tokens`, `cost`).

The QUB-94 rollout was driven by the `BOOP_USE_OPENROUTER_SDK`
flag on the **receiver**. The flag had two values: `0` (legacy
opencode subprocess) and `1` (SDK). After QUB-98 the subprocess
code path is gone; the flag is preserved on the receiver for the
QUB-N rollout mechanism but the runner ignores it.

### Cluster-wide default

Set on the receiver Deployment as an env var:

```yaml
env:
  - name: BOOP_USE_OPENROUTER_SDK
    value: "1"   # post-cutover default; the flag is informational now
```

A flag flip back to `0` is a no-op for the runner (the subprocess
code is gone). To re-enable a real fallback path, ship a runner
image that includes it and redeploy.

### Per-PR override

Add the label `boop:openrouter-sdk` to a PR to opt it into the
SDK path. After the cluster default flipped to `1`, the label is
redundant. The override applies to the next review only — push a
new commit and the flag re-resolves.

The label is honored on the `pull_request` webhook path. The
`issue_comment` (`@BoopPr review`) path uses the cluster
default; per-PR overrides from comments land in a follow-up.

### Smoke testing a single PR

1. Push a commit to a test PR.
2. Within a few seconds, a 🐾 status comment appears. Within
   1-3 minutes, a `## 🐾 Boop's review` summary appears.
3. Verify the runner logs include `path: "openrouter-sdk"` in
   the `opencode/starting` log line (the `opencode` log tag is
   preserved from the pre-SDK era so dashboard log queries
   survive the cutover). The `opencode/exit` line carries
   `mode: "openrouter-sdk"` and the telemetry block
   (`tokens_in`, `tokens_out`, `cost_usd`, `step_count`).

### Diagnostic queries

```
# Show the BOOP_USE_OPENROUTER_SDK value the Job was launched with
kubectl get job -n dev-tools <job-name> -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BOOP_USE_OPENROUTER_SDK")].value}'

# Show the path the runner took (informational after QUB-98 — the
# SDK is the only path)
kubectl logs -n dev-tools -l app=boop,pr-number=<N> --tail=200 | grep '"stage":"opencode"'

# Show the SDK call's telemetry
kubectl logs -n dev-tools -l app=boop,pr-number=<N> --tail=200 | grep '"stage":"opencode".*"msg":"exit"'
```

## End-to-end smoke test

1. Pick a test PR in the `qubitquilt` org (or any repo the App is
   installed on).
2. Push a commit to the PR's branch.
3. Within a few seconds, a 🐾 status comment appears. Within 1-3 minutes, a
   `## 🐾 Boop's review` summary appears with findings and inline
   comments.
4. Push another commit. The status label should be `re-review #2`.
   The summary should review only the diff from the previous head
   (look for the `Re-review #N` line in the prompt and the
   `previous_head_sha:` field in the run logs), not the full
   `main..<head>` range. After the new review posts, any prior
   Boop review threads on lines that are no longer in the diff
   should be auto-resolved, and prior Boop status / summary
   comments should be minimized — the PR thread should now show
   only the new review at the top.
5. Comment `@BoopPr review` on a third push. The status should NOT
   post a 🐾 status comment. The receiver should react 👀 on your
   trigger comment; within 1-3 minutes the runner should add a
   single terminal reaction (🎉 on success, 👎 on failure) on your
   trigger comment. The reaction's "Triggered by @<your-handle>"
   attribution is in the status-comment model that does not apply
   here — the comment + reactions are the user-visible surface
   (QUB-114 reaction mode).
6. Comment `@BoopPr review` again on the same head SHA. The runner
   should NOT re-run; the receiver should post an "Already sniffed"
   reply.
7. To exercise the reaction-only path against a single new head SHA
   when you already have a 🐾 on an earlier review, push a fresh
   commit and add a `@BoopPr review` comment — the receiver skips
   the status comment path entirely and the 👀 appears immediately.

## Test commands

```
# Receiver
cd apps/receiver
make test           # all Go tests
make vet            # go vet ./...

# Runner
cd apps/runner
make build          # syntax check
npm test            # all unit + integration tests

# End-to-end (smoke)
kubectl logs -n dev-tools -l app=boop-receiver --tail=200
kubectl logs -n dev-tools -l app=boop,pr-number=<N> --tail=200
```

## Debugging a stuck review

```
# What Jobs are in flight?
kubectl get jobs -n dev-tools -l app=boop

# Pod logs for a specific PR
PR=<owner>-<repo>-<N>
kubectl logs -n dev-tools -l app=boop,pr-number=<N> --tail=300

# The full Job + pod details
kubectl describe job -n dev-tools boop-<owner>-<repo>-<N>-<sha7>
kubectl describe pod -n dev-tools -l job-name=boop-<owner>-<repo>-<N>-<sha7>
```

If the runner is timing out at 25 min, the Job is hung inside
the OpenRouter SDK call. Re-push to clear (a new head SHA → new
Job). If the Job is hitting the 30-min `activeDeadlineSeconds`,
the SDK call finished but a post-review step is stuck — check
the runner logs for the last `stage` JSON field.

If the receiver is dropping webhooks with 401, the `WEBHOOK_SECRET` in
the cluster does not match the GitHub App config. Compare
`kubectl -n dev-tools get secret boop-secrets -o jsonpath='{.data.WEBHOOK_SECRET}'`
(b64-decoded) to the GitHub App's webhook secret.

## Common dev tasks

| Task | Where |
|---|---|
| Add a new lens | [skills.md](./skills.md#editing-the-skill) |
| Change the review header format | `apps/runner/src/review-header.ts` + `apps/receiver/internal/github/client.go` (mirror both) + their tests |
| Change the dedup key | `apps/receiver/internal/webhook/handler.go` `buildJobName` |
| Change the request grammar | `apps/receiver/internal/webhook/handler.go` `reviewRequestRegex` |
| Add a new env var to the Job | `apps/receiver/internal/webhook/job-template.yaml` + `apps/runner/src/lib/config.ts` `loadConfig` (env destructuring) + `apps/runner/src/index.ts` (ctx field uses) |
| Bump the OpenRouter SDK version | `apps/runner/package.json` |
| Add a new cluster overlay | [deployment.md](./deployment.md#to-add-a-new-cluster) |
| Change a status emoji / label | Both the receiver's `STATUS` (`apps/receiver/internal/webhook/handler.go`) and the runner's `STATUS` (`apps/runner/src/lib/config.ts`). The receiver builds the initial body; the runner appends the timeline. |
| Change a pipeline step | The runner's pipeline is in `apps/runner/src/index.ts` (orchestration) plus `apps/runner/src/lib/*.ts` (steps). New tests belong next to the module they cover (`src/lib/<module>.test.ts`); the integration test in `src/index.test.ts` exercises the wiring. |

## See also

- [architecture.md](./architecture.md) — system flow.
- [deployment.md](./deployment.md) — how it ships.
- [secrets.md](./secrets.md) — credentials.
