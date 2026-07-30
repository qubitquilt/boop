# boop-receiver

Long-lived Go HTTP service. The half of BoopPr that listens.

See also: [README](../apps/receiver/README.md), [architecture](./architecture.md),
[webhook contract](./webhook-contract.md).

## What it does

1. Verify the `X-Hub-Signature-256` HMAC on every request.
2. Filter to `pull_request` events with reviewable actions and to
   `issue_comment` events that ask for a review with `@BoopPr review`.
3. Dedupe by head SHA — re-deliveries for the same head are no-ops.
4. Post a 👀 status comment on the PR (PATCHed later by the runner).
5. Submit a K8s Job that runs `boop-runner` against the PR head.
6. Number every review (`#1`, `#2`, …) so re-reviews get their own header
   instead of rewriting the original.

## Package layout

```
apps/receiver/
├── cmd/receiver/main.go          # entrypoint, config wiring, HTTP server
├── internal/
│   ├── webhook/
│   │   ├── handler.go            # HTTP handler, event dispatch, dedup, Job submit
│   │   ├── reviews.go            # GET /api/reviews: list + group + JSON
│   │   ├── kube.go               # in-cluster / KUBECONFIG client
│   │   ├── template.go           # //go:embed wrapper for the Job template
│   │   ├── job-template.yaml     # the embedded template (live, used at runtime)
│   │   ├── handler_test.go       # buildJobName, shortSHA, renderJobTemplate, dup reply
│   │   ├── reviews_test.go       # collectReviews, reviewFromJob, ListReviews (fake clientset)
│   │   └── verify_test.go        # HMAC verify, isReviewableAction, requestsReview regex
│   └── github/
│       ├── client.go             # App JWT, installation-token cache, PR fetch,
│       │                         #   post/edit comment, react, count prior reviews
│       └── review_header_test.go # ReviewSummaryHeader + IsBoopReviewSummary regex
├── go.mod                        # go 1.23
├── Dockerfile                    # distroless/static, CGO=0
└── Makefile                      # build / test / vet / docker
```

The `apps/k8s/base/job-template.yaml` file is a stale legacy copy; the
live template the receiver renders is the embedded one
(`internal/webhook/job-template.yaml`). Always edit the embedded one.

## HTTP API

| Method | Path            | Purpose                                                                |
|--------|-----------------|------------------------------------------------------------------------|
| POST   | `/webhook`      | GitHub webhook entry point. Always returns 202 (ack) or 4xx/5xx.        |
| GET    | `/health`       | Liveness/readiness. Returns `200 ok`. Used by the K8s probes.           |
| GET    | `/api/reviews`  | Snapshot of boop review Jobs in `TARGET_NAMESPACE`. See below.         |

Ack body shape (always JSON, status 202):

```json
{
  "status": "accepted",
  "detail": "boop-foo-bar-42-a1b2c3d",
  "delivery": "<X-GitHub-Delivery>",
  "ts": "2026-07-29T12:34:56Z"
}
```

`status` is one of: `accepted`, `duplicate`, `ignored`. The
`X-GitHub-Delivery` ID is logged for every event so GitHub's redelivery
tool can be cross-referenced.

### `/api/reviews`

Read-only snapshot of review Jobs for dashboards and ad-hoc inspection.
Lists Jobs in `TARGET_NAMESPACE` with label `app=boop`, groups them by
state, and returns the result as JSON. The K8s API call is filtered
server-side via `LabelSelector=app=boop`, so non-boop Jobs in the
namespace do not pay a serialization cost.

Bucketing rules:

- `active` — `Status.Active > 0`, or has `StartTime` and no
  `CompletionTime` and no `Failed` condition. Includes the moment
  between backoffLimit-exceeded and the Failed condition being written.
- `recent` — `Status.Succeeded > 0` and `CompletionTime` within the
  last 24h.
- `failed` — has a `Failed` condition, or `Status.Failed > 0`, and the
  run started within the last 7d. Older failures age out so the
  dashboard stays focused.

Each list is sorted newest-first by `startTime`. A Job appears in
exactly one bucket.

Response shape:

```json
{
  "active": [
    {
      "name": "boop-qubitquilt-boop-2-b147629",
      "namespace": "dev-tools",
      "owner": "qubitquilt",
      "repo": "boop",
      "pr": 2,
      "commit": "b147629deadbeefcafebabe...",
      "baseRef": "main",
      "startTime": "2026-07-29T11:54:00Z",
      "status": "Running",
      "active": 1,
      "succeeded": 0,
      "failed": 0
    }
  ],
  "recent": [ /* same shape, status "Complete" */ ],
  "failed": [ /* same shape, status "Failed", duration set */ ]
}
```

The endpoint is unauthenticated. It is intended to be reachable only
from inside the cluster (e.g., via `kubectl port-forward`); if exposed
beyond the cluster, front it with an `IngressRoute` + basic auth or a
Tailscale-only entrypoint. It reveals Job metadata only — no PR
content, no secrets.

## Configuration

Read from env vars at startup. All required. See
[secrets.md](./secrets.md) for sourcing.

| Var | Required | Default | Purpose |
|---|---|---|---|
| `WEBHOOK_SECRET` | yes | — | HMAC secret for `X-Hub-Signature-256`. Must match the GitHub App webhook config. |
| `GITHUB_APP_ID` | yes | — | App ID (integer). |
| `GITHUB_APP_INSTALLATION_ID` | yes | — | Installation ID (integer) for the target org. |
| `GITHUB_APP_PRIVATE_KEY` | yes | — | PEM-encoded PKCS#1 RSA key. |
| `PORT` | no | `8080` | HTTP listen port. |
| `JOB_IMAGE` | no | `ghcr.io/michaelruelas/boop-runner:latest` | Image the receiver submits in Job pods. In-cluster the `apps/k8s/base/config.yaml` overrides to `ghcr.io/qubitquilt/boop-runner:stable`. |
| `TARGET_NAMESPACE` | no | `dev-tools` | Namespace to submit Jobs into. |
| `BOT_LOGIN` | no | (empty) | If set, the receiver drops `issue_comment` events from this sender (skips self-mentions). Empty disables. |
| `LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. JSON to stdout. |

## Event handling

### `pull_request`

Reviewable actions: `opened`, `reopened`, `synchronize`, `ready_for_review`.
Anything else (`closed`, `edited`, `assigned`, …) is acked `ignored` and
discarded.

On a reviewable action, the receiver:

1. Builds the Job name from the head SHA: `boop-<owner>-<repo>-<number>-<sha7>`.
2. Calls `claimJobSlot` to dedupe (see below).
3. Computes the review number by counting prior `## 🐾 Boop's …` summary
   comments on the PR. The next review is `count + 1`.
4. Posts the status comment with the review label.
5. Submits the Job.

### `issue_comment`

Triggered when a user comments `@BoopPr review` (or one of the natural
phrasings — see [request grammar](./webhook-contract.md#request-grammar))
on a PR. The receiver:

1. Filters to `action=created` and PR-only (`issue.pull_request` set).
2. Drops self-comments when `BOT_LOGIN` is set.
3. Verifies the comment actually requests a review (regex match).
4. Fetches the PR to learn the current head SHA + base ref.
5. Dedupes by Job name.
6. Reacts to the comment with 👀 (only after dedup says we will run).
7. Posts the status comment with the trigger attribution.
8. Submits the Job.

Bare mentions and references to "review" (e.g. "the prior review was
great") do **not** trigger.

## Dedup (`claimJobSlot`)

The Job name encodes the head SHA. The K8s API is the source of truth for
"is there already a run for this head":

| Existing Job status | Action |
|---|---|
| `missing` (no Job) | Submit a new Job. |
| `active` (running) | Ack `duplicate`. Do not re-submit. |
| `succeeded` (already done) | Ack `duplicate`. For `issue_comment` triggers, post a "Already sniffed `<sha>`" reply. |
| `failed` | Delete the failed Job (background propagation), submit a new one. |

This runs **before** posting the status comment. A duplicate delivery
must not leave a stranded 👀.

## Job template (the embedded one)

`apps/receiver/internal/webhook/job-template.yaml` is the live Job
manifest rendered per request. Placeholders are `__OWNER__`, `__REPO__`,
`__NUMBER__`, `__SHA__`, `__SHA7__`, `__BASE_REF__`, `__IMAGE__`,
`__STATUS_COMMENT_ID__`, `__REACTION_COMMENT_ID__`, `__REVIEW_NUMBER__`.
Double-underscore form survives prettier without being rewritten as
JSX-style braces.

The template wires:

- **Env:** `PR_OWNER`, `PR_REPO`, `PR_NUMBER`, `PR_HEAD_SHA`,
  `PR_BASE_REF` (from the PR payload); `GITHUB_APP_ID`,
  `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY`,
  `OPENROUTER_API_KEY` (from `boop-secrets`); `BOOP_STATUS_COMMENT_ID`,
  `BOOP_REACTION_COMMENT_ID`, `BOOP_REVIEW_NUMBER` (from the request);
  `BOOP_SKIP_SKILL=0` (debug-only escape hatch).
- **Volume mount:** `boop-runner-config` ConfigMap at
  `/home/opencode/.config/opencode` (read-only).
- **Security context:** `runAsUser=1000`, `runAsNonRoot=true`,
  `fsGroup=1000`. Matches the `ubuntu` user baked into the runner image.
- **Resource requests/limits:** 1 CPU / 2 GiB → 4 CPU / 6 GiB.
- **Lifecycle:** `backoffLimit=1`, `activeDeadlineSeconds=1800`,
  `ttlSecondsAfterFinished=3600`, `restartPolicy=Never`.

The template's `items` list in the volume must stay in lockstep with
`apps/k8s/base/kustomization.yaml` `configMapGenerator.files` and the
`apps/k8s/base/runner-config/` directory. Adding a new lens requires all
three updates.

## GitHub App client (`internal/github`)

Thin wrapper around `github.com/google/go-github/v68` with the App auth
flow:

- `appJWT(now)` mints a 10-min JWT signed with the App's RSA private key
  (30s leeway on `iat`).
- `installationClient(ctx)` returns a `*github.Client` authenticated as
  the installation. Caches the token until (expiry − 5 min). The cache is
  guarded by a `sync.Mutex`; safe for concurrent use.
- `FetchPullRequest(ctx, owner, repo, n)` returns the head SHA + base ref
  + title — what the receiver needs to build a Job when triggered by
  `issue_comment`.
- `PostIssueComment`, `UpdateIssueComment`, `AddCommentReaction` —
  surface used by the status thread.
- `CountPriorReviews(ctx, owner, repo, n)` — paginates issue comments,
  counts those whose body starts with `## 🐾 Boop's …`. Used to compute
  the next review number. Falls back to 1 on GitHub API errors so a
  transient hiccup never blocks a review.

### Review header regex (must stay in lockstep with the runner)

`priorReviewHeaderRegex` matches `## 🐾 Boop's review`,
`## 🐾 Boop's re-review`, `## 🐾 Boop's re-review #2`, etc. The
companion `ReviewSummaryHeader(n)` formats them. The runner's
`apps/runner/src/review-header.mjs` mirrors both. Tests on both sides
pin the format.

## Tests

```
cd apps/receiver
make test     # go test ./...
make vet      # go vet ./...
```

Coverage by file:

- `internal/webhook/verify_test.go` — HMAC verify, `isReviewableAction`,
  `requestsReview` (extensive positive/negative regex cases).
- `internal/webhook/handler_test.go` — `buildJobName`, `shortSHA`,
  `duplicateReviewReply`, full `renderJobTemplate` round-trip including
  the new `BOOP_REVIEW_NUMBER` wiring.
- `internal/webhook/reviews_test.go` — `collectReviews` grouping +
  windowing, `reviewFromJob` with full and sparse labels,
  `humanStatus` table, end-to-end `ListReviews` via a fake clientset
  (label selector excludes non-boop Jobs), 503 path on K8s List error.
- `internal/github/review_header_test.go` — `ReviewSummaryHeader(n)` for
  `n=0,1,2,3,10`; `IsBoopReviewSummary` for the live format plus tolerant
  historical forms.

## Local run

```
cd apps/receiver
make build                   # ./bin/receiver
export WEBHOOK_SECRET=$(openssl rand -hex 32)
export KUBECONFIG=~/.kube/config
./bin/receiver
```

For real GitHub events, use smee.io to forward webhooks to
`http://localhost:8080/webhook`.

## Build

```
make docker-build IMAGE=ghcr.io/qubitquilt/boop-receiver:dev
make docker-push
```

The image is multi-stage: `golang:1.23-alpine` builder, distroless
runtime, non-root. The binary is `CGO_ENABLED=0` for static linking.

## Failure modes

- **Signature verify fails.** 401 + log line. The request never touches
  the K8s API.
- **Body > 1 MiB.** 400 + log. The receiver caps at `1<<20` bytes.
- **K8s API down.** 500 + log. The webhook is dropped, GitHub will
  redeliver.
- **GitHub API down (issue_comment).** 502 + log. Same as above; the
  K8s API and GitHub API are both external dependencies.
- **Render template fails.** 500 + log. The template should not fail at
  runtime; only plausible cause is a malformed embed.
- **Create job fails.** 500 + log. Most often RBAC drift.

## See also

- [architecture.md](./architecture.md) — system-level flow.
- [webhook-contract.md](./webhook-contract.md) — request grammar, dedup
  edge cases, status thread semantics.
- [deployment.md](./deployment.md) — how this gets deployed.
- [secrets.md](./secrets.md) — credential sourcing.
