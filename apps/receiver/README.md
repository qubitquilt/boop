# boop-receiver

GitHub webhook receiver for the boop agent. Validates HMAC, filters
`pull_request` events, and submits a Kubernetes Job per PR.

The GitHub App posting reviews is named **BoopPr** (the internal project
is `boop`).

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
