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

- **Multi-lens review** — runs a configurable set of reviewer lenses
  (code quality, error handling, design patterns, readability, SOLID)
  against every PR head.
- **Line-specific inline comments** — actionable feedback pinned to
  the diff, not a wall of prose in a summary.
- **Single summary post** — one short comment per PR with the headline
  findings.
- **At-mention re-review** — drop `@BoopPr` on any PR comment to
  re-trigger him.
- **Friendly status updates** — a "Boop is reviewing…" comment is
  posted up front and patched as the review progresses.

## How it works

```
PR opened ──▶ boop-receiver (Go) ──▶ K8s Job (boop-runner, Node)
                                          │
                                          ├── clone PR
                                          ├── run multi-lens review
                                          ├── post summary comment
                                          └── post inline comments
```

- `apps/receiver/` — Go HTTP server. Validates the GitHub webhook,
  filters reviewable events, submits a Job per PR.
- `apps/runner/` — Node.js one-shot worker. Clones the PR, runs the
  `boop` skill via OpenCode, posts the result.
- `apps/k8s/base/` — Kustomize manifests (cluster-agnostic).
- `apps/k8s/overlays/<cluster>/` — per-cluster namespace + image
  tags. `pugquilt` is the home cluster.

## Image publishing

GitHub Actions on push to `main` or a `v*` tag:

- Push to `main` → builds and pushes `:latest` to `ghcr.io/qubitquilt/boop-{receiver,runner}`
- Push tag `vX.Y.Z` → builds and pushes `:stable`, `:vX.Y.Z`, `:X.Y`, `:X`

The default overlay pins `:stable`. Bump to a specific tag (`:v0.1.0`)
for reproducible rollouts.

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
