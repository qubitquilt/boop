# boop-runner

The half of BoopPr that does the work. One-shot runner that reviews a
PR. Invoked as a Kubernetes Job by the boop-receiver when a
`pull_request` or `issue_comment` event with `@BoopPr` arrives.

## What it does

1. Mints a GitHub App installation token from the App credentials in
   the environment.
2. Clones the PR at `PR_HEAD_SHA` into `/work/repo`.
3. Starts OpenCode against `/home/opencode/.config/opencode` (mounted
   from the `boop-runner-config` ConfigMap), runs the `boop` skill
   on the repo, and captures the assistant's text output.
4. Posts the result as a single review comment on the PR.

## Build

```
make docker-build
```

or:

```
docker buildx build --platform linux/arm64 \
  -t ghcr.io/michaelruelas/boop-runner:latest --push .
```

## Required env (provided by the Job)

| Var | Source |
|---|---|
| `GITHUB_APP_ID` | `boop-secrets` |
| `GITHUB_APP_INSTALLATION_ID` | `boop-secrets` |
| `GITHUB_APP_PRIVATE_KEY` | `boop-secrets` |
| `PR_OWNER`, `PR_REPO`, `PR_NUMBER` | Job template |
| `PR_HEAD_SHA`, `PR_BASE_REF` | Job template |
| `OPENROUTER_API_KEY` | `boop-secrets` |
