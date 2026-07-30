# boop

PR review bot for the qubitquilt org. Receives GitHub webhooks, runs a
multi-lens code review, posts results back to the PR.

## Layout

- `apps/receiver/` — Go HTTP server, validates webhooks, creates runner Jobs
- `apps/runner/` — Node.js review worker, runs the boop skill, posts the review
- `apps/k8s/base/` — Kustomize manifests (cluster-agnostic)
- `apps/k8s/overlays/<cluster>/` — per-cluster namespace + image tags

## Image publishing

GitHub Actions on push to `main` or a `v*` tag:

- Push to `main` → builds and pushes `:latest` to `ghcr.io/qubitquilt/boop-{receiver,runner}`
- Push tag `vX.Y.Z` → builds and pushes `:stable`, `:vX.Y.Z`, `:X.Y`, `:X`

The default overlay pins `:stable`. Bump to a specific tag (`:v0.1.0`) for reproducible deployments.

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
