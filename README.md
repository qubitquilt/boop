# 🐾 BoopPr

> A loyal little pug who reviews your pull requests.

BoopPr (or just **Boop** to his friends) is the GitHub App that reviews PRs
across the `qubitquilt` org. He sniffs out bugs, enforces style, leaves
gentle inline suggestions, and only ever approves the work he would
vouch for. Your human reviewers get a head start; you get a good boy on
the job.

The internal project here is `boop`. The GitHub App on the wire is
`BoopPr`. Same pug, different name tag.

## What Boop does

- **Multi-expert review** — one LLM walkthrough + N parallel
  expert calls (one per lens), then a narrator that
  synthesizes them. Seven lenses: code quality, design
  pattern, error handling, readability, SOLID, test quality,
  deep. Each expert sees the same walkthrough as shared
  context, so the findings read as one voice.
- **Persona** — light pug flourishes sampled from a curated
  pool. Used once per review in the summary's TL;DR,
  "What this PR does well", or the line after the closing
  signal. Never in inline comment bodies. See
  [`resources/persona.md`](./apps/k8s/base/runner-config/skills/boop/resources/persona.md).
- **Line-specific inline comments** — actionable feedback
  pinned to the diff, not a wall of prose in a summary.
- **Single summary post** — one short comment per PR with
  the headline findings.
- **Status thread (PR-opened triggers)** — a 🐾 "Boop's on
  the case!" comment is posted up front and PATCHed at each
  stage. Final stage is 🦴 on done, ❌ on failure.
- **Reaction mode (PR-comment triggers)** — when a user
  re-triggers via `@BoopPr review` on a comment, Boop
  reacts 👀 on the trigger and adds a single terminal
  reaction (🎉 on done, 👎 on failed) instead of a status
  thread. One reaction change, one notification.
- **At-mention re-review** — drop `@BoopPr review` on any
  PR comment to re-trigger him.
- **Re-review delta** — the runner diffs
  `previous_head_sha..head_sha`, not the full PR base.
- **rtk compression (QUB-85)** — file reads from the
  skill ConfigMap route through rtk with a raw
  `fs.readFile` fallback when the binary is missing.

## Documentation

Full documentation lives in [`docs/`](./docs/README.md):

- [docs/README.md](./docs/README.md) — top-level map, one-screen overview.
- [docs/product.md](./docs/product.md) — public perspective, what the PR author sees.
- [docs/architecture.md](./docs/architecture.md) — system flow, components, failure modes.
- [docs/webhook-contract.md](./docs/webhook-contract.md) — events, dedup, status thread, output format.
- [docs/receiver.md](./docs/receiver.md) — Go webhook receiver (scoped).
- [docs/runner.md](./docs/runner.md) — Node PR-review worker (scoped).
- [docs/skills.md](./docs/skills.md) — the boop review skill + seven lenses.
- [docs/deployment.md](./docs/deployment.md) — K8s overlays, CI, image tags, ArgoCD, release lifecycle.
- [docs/secrets.md](./docs/secrets.md) — GitHub App + OpenBao wiring.
- [docs/development.md](./docs/development.md) — local dev, build, test, debug.

## How it works

```
 PR opened ──▶ boop-receiver (Go) ──▶ K8s Job (boop-runner, Node)
                                            │
                                            ├── mints GitHub App install token
                                            ├── clones PR
                                            ├── walkthrough           (1 LLM call)
                                            ├── classify              (1 LLM call)
                                            ├── N × experts in parallel  (N LLM calls)
                                            ├── gather / meta-review  (1 LLM call)
                                            ├── narrate               (1 LLM call)
                                            ├── ste-lint              (mechanical, best-effort)
                                            ├── post summary comment
                                            └── post inline comments
```

- `apps/receiver/` — Go HTTP server. Validates the GitHub
  webhook, filters reviewable events, submits a Job per
  PR. Persists every run + telemetry to SQLite
  (see [secrets.md](./docs/secrets.md); optional via
  `DB_PATH`).
- `apps/runner/` — Node.js one-shot worker. Clones the PR,
  runs the multi-expert pipeline via the OpenRouter SDK
  in-process, posts the result. File reads go through the
  [rtk adapter](./docs/runner.md#rtk-adapter-qub-85)
  (QUB-85).
- `apps/k8s/base/` — Kustomize manifests (cluster-agnostic).
- `apps/k8s/overlays/<cluster>/` — per-cluster namespace
  + image tags. `pugquilt` is the home cluster.

## Image publishing

GitHub Actions on push to `main` or a `v*` tag:

- Push to `main` → builds and pushes `:latest` to `ghcr.io/qubitquilt/boop-{receiver,runner}`
- Push tag `vX.Y.Z` → builds and pushes `:stable`, `:vX.Y.Z`, `:X.Y`, `:X`

The default overlay pins `:latest`. Bump to `:stable` or a specific tag
(`:v0.1.0`) for reproducible rollouts.

## Local dev

Receiver:
```sh
cd apps/receiver
make test    # go test ./...
make build   # go build -o bin/receiver ./cmd/receiver
```

Runner:
```sh
cd apps/runner
make build   # node --check src/index.mjs
```

## Deploy

ArgoCD Application in the cluster repo points at this repo:

```yaml
source:
  repoURL: https://github.com/qubitquilt/boop.git
  path: apps/k8s/overlays/<cluster-name>
```

## Required secrets (via OpenBao ClusterSecretStore)

Stored at `secret/dev-tools/boop` in OpenBao:
- `webhook-secret` — matches the GitHub webhook secret
- `app-id` — GitHub App ID
- `installation-id` — GitHub App installation ID for the target org
- `private-key` — GitHub App private key (PEM)
- `openrouter-api-key` — for the LLM

---

Give the codebase the good boy it deserves. 🐾
