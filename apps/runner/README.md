# boop-runner

The half of BoopPr that does the work. One-shot runner that reviews a
PR. Invoked as a Kubernetes Job by the boop-receiver when a
`pull_request` or `issue_comment` event with `@BoopPr` arrives.

## What it does

1. Mints a GitHub App installation token from the App credentials in
   the environment.
2. Clones the PR at `PR_HEAD_SHA` into `/work/repo`.
3. Reads the boop skill (`SKILL.md` + the seven lens files) from
   `/home/opencode/.config/opencode` (the `boop-runner-config`
   ConfigMap mount) and inlines them into a prompt. Reads go through
   the [rtk adapter](https://github.com/qubitquilt/boop/blob/main/docs/runner.md#rtk-adapter-qub-85)
   (QUB-85) for compression; the adapter falls back to raw
   `fs.readFile` when rtk is disabled or the binary is missing.
4. Calls the OpenRouter SDK in-process to produce the review. The
   response is parsed for the `=== SUMMARY === / === INLINE COMMENTS
   === / === CONFIDENCE === / === END ===` block and posted to the
   PR.
5. Posts the result as a single review comment on the PR.

## Build

```
make docker-build
```

or:

```
docker buildx build --platform linux/arm64 \
  -t ghcr.io/michaelruelas/boop-runner:latest --push .
```

The Dockerfile supports `linux/amd64` and `linux/arm64`; the
per-arch rtk tarball is selected from the GitHub release via
`TARGETARCH` and a pinned SHA-256 verifies the download.

## Required env (provided by the Job)

| Var | Source |
|---|---|
| `GITHUB_APP_ID` | `boop-secrets` |
| `GITHUB_APP_INSTALLATION_ID` | Job template (`__INSTALLATION_ID__` from `X-GitHub-Installation-ID` header) |
| `GITHUB_APP_PRIVATE_KEY` | `boop-secrets` |
| `PR_OWNER`, `PR_REPO`, `PR_NUMBER` | Job template |
| `PR_HEAD_SHA`, `PR_BASE_REF` | Job template |
| `OPENROUTER_API_KEY` | `boop-secrets` |
| `OPENROUTER_MODEL` | Job template (model id, e.g. `minimax/minimax-m3`) |
| `BOOP_RTK_DISABLED` | optional; `1` bypasses the rtk adapter (pre-QUB-85 reads) |
