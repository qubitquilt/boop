# boop-runner

The half of BoopPr that does the work. One-shot runner that reviews a
PR. Invoked as a Kubernetes Job by the boop-receiver when a
`pull_request` or `issue_comment` event with `@BoopPr` arrives.

## What it does

1. Mints a GitHub App installation token from the App credentials in
   the environment.
2. Clones the PR at `PR_HEAD_SHA` into `/work/repo`.
3. Reads the boop skill (`SKILL.md` + the seven lens files + the
   persona resource) from the ConfigMap mount. Reads go through the
   [rtk adapter](#rtk-adapter-qub-85) (QUB-85) for compression;
   the adapter falls back to raw `fs.readFile` when rtk is
   disabled or the binary is missing.
4. Runs the multi-expert pipeline:
   - **walkthrough** — one LLM call generates a human-readable
     PR summary that every expert consumes as shared context.
   - **classify** — one LLM call tags the PR type so dispatch
     can pick the right experts.
   - **N experts in parallel** — one LLM call per active lens,
     with the lens file as system prompt and walkthrough + diff
     as user message.
   - **gather** — de-duplicates the findings in-process.
   - **meta-review** — one LLM call that may request a bounded
     re-pass of experts whose findings "stick out as
     potentially wrong" (one re-pass per run, no loops).
   - **narrate** — one final LLM call that synthesizes the
     walkthrough and the (re-reviewed) findings into the
     structured `=== SUMMARY === / === INLINE COMMENTS ===
     / === END ===` block.
5. Runs `lib/ste-lint.ts` mechanically on the LLM output
   (best-effort; drift is logged, not re-fed to the LLM).
6. Posts the summary as a single PR comment and the inline
   comments as line-pinned review comments.

On comment-triggered runs (`BOOP_NO_STATUS_COMMENT=1`,
QUB-114), the runner does not post or PATCH a status
comment; it adds a single terminal reaction (🦴 on done,
❌ on failed) on the trigger comment.

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
| `BOOP_NO_STATUS_COMMENT` | Job template; `1` triggers reaction mode (QUB-114) |
| `BOOP_RTK_DISABLED` | optional; `1` bypasses the rtk adapter (pre-QUB-85 reads) |
