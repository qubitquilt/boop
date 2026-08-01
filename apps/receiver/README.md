# boop-receiver

The half of BoopPr that listens. GitHub webhook receiver for the
BoopPr GitHub App: validates HMAC, filters `pull_request` and
`issue_comment` events, and submits a Kubernetes Job per PR.

The GitHub App posting reviews is **BoopPr**; this internal project
is `boop`. Same pug, different name tag.

## Build

```
make build
```

## Test

```
make test
```

## Container

```
make docker-build docker-push IMAGE=ghcr.io/michaelruelas/boop-receiver:latest
```

## Run locally

Requires a kubeconfig and a real `WEBHOOK_SECRET`:

```
export WEBHOOK_SECRET=$(openssl rand -hex 32)
export KUBECONFIG=~/.kube/config
make run
```

## Endpoints

- `POST /webhook` — entry point for the GitHub App webhook
- `GET /health` — liveness/readiness
- `GET /api/reviews` — snapshot of in-flight and recent review Jobs
- `GET /api/installations` — GitHub App installations (data layer)
- `GET /api/runs` — paginated run history (data layer)
- `GET /api/stats` — aggregations for the dashboard (data layer)
- `POST /api/runs/{id}/telemetry` — runner reports final token + cost
- `POST /api/runs/{id}/status` — runner reports lifecycle transitions

The data-layer endpoints need a writable `DB_PATH` (default
`/data/boop.db`); the runner POST endpoints additionally need a
`RUNNER_TOKEN` env var. See [docs/receiver.md](../../docs/receiver.md)
for the full contract.
