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
