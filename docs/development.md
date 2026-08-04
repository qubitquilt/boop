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
make install        # npm install
make build          # node --check src/index.mjs (syntax check)
make test           # bun test src/*.test.mjs src/lib/*.test.mjs
```

`make test` runs under Bun. `make test-node` falls back to
`node --test` for the rare case Bun disagrees with node on a test.
See [QUB-10](https://linear.app/qubit-quilt/issue/QUB-10/convert-runner-to-using-bun)
for the rationale.

### Run locally (no cluster, no LLM)

For the header format and other pure helpers, the test suite is the only check:

```
cd apps/runner
make test                                              # all tests, under Bun
bun test src/review-header.test.mjs                    # just the header fixtures
bun test src/lib/github.test.mjs                       # GitHub API surface
node --test src/lib/opencode.test.mjs                     # parser, prompt builder
```

### Run locally (against a real PR, outside a Job)

The runner is designed to run inside a K8s Job; all its env vars
(PR_OWNER, PR_REPO, PR_NUMBER, PR_HEAD_SHA, PR_BASE_REF, GITHUB_APP_*,
OPENROUTER_API_KEY, BOOP_*) are set by the Job template. To run it
locally, set them by hand:

```
export GITHUB_APP_ID=...
export GITHUB_APP_PRIVATE_KEY="$(cat /path/to/key.pem)"
export PR_OWNER=qubitquilt
export PR_REPO=homelab-infra
export PR_NUMBER=42
export PR_HEAD_SHA=<sha>
export PR_BASE_REF=main
export OPENROUTER_API_KEY=...

# Optional; usually set by the Job template
export BOOP_STATUS_COMMENT_ID=
export BOOP_REACTION_COMMENT_ID=
export BOOP_REVIEW_NUMBER=1
export BOOP_SKIP_SKILL=0   # 1 = minimal prompt smoke test

cd apps/runner
node src/index.mjs
```

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

For a faster preview, run the runner locally with the edited
`opencode.json` and lens files materialized into a writable dir (the
runner does this internally; for local, point `HOME` and
`XDG_CONFIG_HOME` at a copy).

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

## End-to-end smoke test

1. Pick a test PR in the `qubitquilt` org (or any repo the App is
   installed on).
2. Push a commit to the PR's branch.
3. Within a few seconds, a 🐾 status comment appears. Within 1-3 minutes, a
   `## 🐾 Boop's review` summary appears with findings and inline
   comments.
4. Push another commit. The status label should be `re-review #2`.
   The summary should review only the diff from the previous head
   (look for the `Previous review head SHA: …` line in the opencode
   logs), not the full `main..<head>` range. After the new review
   posts, any prior Boop review threads on lines that are no longer
   in the diff should be auto-resolved, and prior Boop status /
   summary comments should be minimized — the PR thread should now
   show only the new review at the top.
5. Comment `@BoopPr review` on a third push. The status should show
   `Triggered by @<your-handle>`.
6. Comment `@BoopPr review` again on the same head SHA. The runner
   should NOT re-run; the receiver should post a "Already sniffed"
   reply.

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

If the runner is timing out at 25 min, the Job is hung inside the
OpenCode call. Re-push to clear (a new head SHA → new Job). If the
Job is hitting the 30-min `activeDeadlineSeconds`, the opencode call
finished but a post-review step is stuck — check the runner logs for
the last `stage` JSON field.

If the receiver is dropping webhooks with 401, the `WEBHOOK_SECRET` in
the cluster does not match the GitHub App config. Compare
`kubectl -n dev-tools get secret boop-secrets -o jsonpath='{.data.WEBHOOK_SECRET}'`
(b64-decoded) to the GitHub App's webhook secret.

## Common dev tasks

| Task | Where |
|---|---|
| Add a new lens | [skills.md](./skills.md#editing-the-skill) |
| Change the review header format | `apps/runner/src/review-header.mjs` + `apps/receiver/internal/github/client.go` (mirror both) + their tests |
| Change the dedup key | `apps/receiver/internal/webhook/handler.go` `buildJobName` |
| Change the request grammar | `apps/receiver/internal/webhook/handler.go` `reviewRequestRegex` |
| Add a new env var to the Job | `apps/receiver/internal/webhook/job-template.yaml` + `apps/runner/src/lib/config.mjs` `loadConfig` (env destructuring) + `apps/runner/src/index.mjs` (ctx field uses) |
| Bump the opencode version | `apps/runner/package.json` |
| Add a new cluster overlay | [deployment.md](./deployment.md#to-add-a-new-cluster) |
| Change a status emoji / label | Both the receiver's `STATUS` (`apps/receiver/internal/webhook/handler.go`) and the runner's `STATUS` (`apps/runner/src/lib/config.mjs`). The receiver builds the initial body; the runner appends the timeline. |
| Change a pipeline step | The runner's pipeline is in `apps/runner/src/index.mjs` (orchestration) plus `apps/runner/src/lib/*.mjs` (steps). New tests belong next to the module they cover (`src/lib/<module>.test.mjs`); the integration test in `src/index.test.mjs` exercises the wiring. |

## See also

- [architecture.md](./architecture.md) — system flow.
- [deployment.md](./deployment.md) — how it ships.
- [secrets.md](./secrets.md) — credentials.
