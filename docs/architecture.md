# Architecture

End-to-end view of how a webhook becomes a PR review.

## Topology

```
                                  ┌─────────────────┐
                                  │  OpenRouter LLM │
                                  └────────▲────────┘
                                           │ HTTPS
                                           │
   GitHub (PR / comment)                    │
        │ webhook                           │
        ▼                                   │
┌──────────────────┐  submit Job    ┌───────┴──────┐
│  boop-receiver   │ ──────────────▶│  boop-runner │
│  (Deployment,    │                │  (Job,       │
│   1 replica,     │                │   1 pod / PR)│
│   in-cluster)    │                │              │
│                  │                │              │
│  - HMAC verify   │                │  - clone PR  │
│  - filter events │                │  - run skill │
│  - dedupe by SHA │                │  - post 📝   │
│  - post 👀       │                │  - post 📌   │
│  - mint comments │                │              │
└────────▲─────────┘                └───────▲──────┘
         │                                  │
         │     ┌────────────────────────┐   │
         └──── │  Kubernetes API server │ ──┘
               │  (in-cluster)          │
               └────────────────────────┘
```

Two services, one pipeline. The receiver is a long-lived HTTP server. The
runner is a one-shot Kubernetes Job per PR.

## Components

### boop-receiver (`apps/receiver/`)

Long-lived. Validates HMAC, filters events, dedupes by head SHA, posts the
"reviewing…" status comment, submits a Job. Stateless across requests —
all state lives in the K8s API and the GitHub API.

- **Image:** `ghcr.io/qubitquilt/boop-receiver` (Go 1.23, distroless).
- **Replicas:** 1 (eventually consistent — duplicates are deduped by head
  SHA, not by receiver instance).
- **Endpoints:** `POST /webhook`, `GET /health`.
- **Cluster scope:** namespace `dev-tools`. Creates Jobs in the same
  namespace.
- **GitHub App auth:** uses the App creds to post the "reviewing…"
  comment and to fetch PR metadata for `@BoopPr review` triggers.

See [receiver.md](./receiver.md).

### boop-runner (`apps/runner/`)

Short-lived. One pod per PR review. Clones the PR, runs the `boop` skill
via OpenCode, posts the result.

- **Image:** `ghcr.io/qubitquilt/boop-runner` (Ubuntu 24.04, Node 22,
  `opencode-ai` from npm).
- **Lifetime:** 1 pod, started by the receiver, runs to completion or 30
  min (`activeDeadlineSeconds: 1800`), GC'd 1 h after finish.
- **Workspace:** `/work/repo` (the cloned PR) and `/tmp/opencode-*`
  (writable copies of the read-only config mount).
- **No outbound network** except: GitHub (clone + API), OpenRouter (LLM).
- **OpenCode config:** mounted from the `boop-runner-config` ConfigMap at
  `/home/opencode/.config/opencode`. Read-only. Materialized to
  `/tmp/opencode-config/opencode/` for OpenCode's TUI.

See [runner.md](./runner.md).

### Skills & config ConfigMap

`apps/k8s/base/runner-config/` packs into the `boop-runner-config`
ConfigMap. Contents:

| Key | Path | Purpose |
|---|---|---|
| `opencode.json` | `/home/opencode/.config/opencode/opencode.json` | model, provider |
| `skill-boop` | `…/skills/boop/SKILL.md` | orchestrator |
| `skill-boop-agent-code-quality` | `…/skills/boop/agents/review-code-quality.md` | lens 1 |
| `skill-boop-agent-design-pattern` | `…/skills/boop/agents/review-design-pattern.md` | lens 2 |
| `skill-boop-agent-error-handling` | `…/skills/boop/agents/review-error-handling.md` | lens 3 |
| `skill-boop-agent-readability` | `…/skills/boop/agents/review-readability.md` | lens 4 |
| `skill-boop-agent-solid-principles` | `…/skills/boop/agents/review-solid-principles.md` | lens 5 |
| `skill-boop-agent-test-quality` | `…/skills/boop/agents/review-test-quality.md` | lens 6 |
| `skill-boop-agent-deep` | `…/skills/boop/agents/review-deep.md` | lens 7 |
| `skill-gh-cli` | `…/skills/gh-cli/SKILL.md` | `gh` CLI reference (unused at runtime today) |

The receiver's embedded Job template also declares these as `items` for
the `runner-config` volume (so each key lands at the right path on mount).

ConfigMap limit: 1 MB (etcd hard cap). The current payload is well under.
Bumping against the limit would require moving to a git-sync init
container pattern.

See [skills.md](./skills.md).

## Request lifecycle

```
T+0s   GitHub delivers a pull_request webhook to /webhook
T+0s   Receiver verifies X-Hub-Signature-256 (HMAC-SHA256, constant-time)
T+0s   Receiver parses, filters, dedupes by Job name (= head SHA)
T+0s   Receiver posts 👀 status comment (and PATCHes in trigger attribution
       + review label for issue_comment triggers)
T+0s   Receiver renders Job template, submits Job
T+1s   Job pod starts; runner mints installation token
T+1s   runner PATCHes status → 🔐 authenticated
T+2s   runner clones PR (`git clone --depth 50` + `fetch --depth 200`)
T+5s   runner PATCHes status → 📥 fetched
T+5s   runner builds prompt (orchestrator + 7 lenses inlined) and calls
       `opencode run` (with PTY via `script -qfc`)
T+5s   runner PATCHes status → 🧠 reviewing
T+60s..120s   opencode returns; runner strips ANSI, parses output
T+60s..120s   runner PATCHes status → ✅ review in (after summary + inlines)
T+fail   on any error: PATCH status → ❌ distracted (with details)
```

Total wall-clock for a typical PR: 1-3 minutes. Dominated by the LLM
call. LLM calls have a 25-min hard ceiling; the Job's
`activeDeadlineSeconds` is 30 min, leaving 5 min of headroom for
post-review work.

## State

State lives in three places:

1. **Kubernetes API.** The Job object is the deduplication unit. Its
   name encodes the head SHA (`boop-<owner>-<repo>-<number>-<sha7>`). Its
   status (`active` / `succeeded` / `failed` / `missing`) drives the
   dedup logic.
2. **GitHub API.** The status comment, the summary comment, and the
   inline comments. Each is keyed by its own GitHub ID. The status
   comment ID is passed to the runner via `BOOP_STATUS_COMMENT_ID` so it
   can PATCH the same one the receiver created.
3. **In-memory caches.** Both services cache GitHub App installation
   tokens for up to (token expiry − 5 min). On cache miss, a new App JWT
   is minted and exchanged. No persistent local state.

The receiver and runner share no state directly. The handoff is the Job
spec the receiver submits; everything after that is a clean pod start.

## Failure modes

| Failure | Effect | Recovery |
|---|---|---|
| Webhook signature invalid | 401, request dropped | Re-deliver from GitHub; check `WEBHOOK_SECRET` |
| Job already `active` for head SHA | 202 `duplicate` ack; no new Job | Job will post when it finishes; re-reviews need a new SHA |
| Job already `succeeded` for head SHA | 202 `duplicate` ack; for `@BoopPr review` triggers, a short "Already sniffed" PR reply is posted | Push a new commit to trigger a fresh review |
| Job already `failed` for head SHA | Old Job is deleted, a new one is submitted | Re-delivery from GitHub will hit the same path; transient K8s issues clear on the next event |
| Token mint fails | 502 from receiver (`/webhook` for issue_comment) or 500 (Job fails) | Re-deliver; check `GITHUB_APP_PRIVATE_KEY` and `installation-id` |
| LLM times out | `postStatus("failed")`, Job exits non-zero | The 25-min hard ceiling means a hung call is killed; the failure is visible in the status thread |
| LLM returns empty stdout | Job throws, status is `failed` with detail | Same; the runner exits 1, the pod is GC'd by the TTL |
| ConfigMap missing / unreadable | Status thread sees a `failed` with `lens … read attempt N failed` detail | The skill body is read with retry + 1s backoff to absorb transient `..data` symlink races right after pod start |

## Concurrency

- **Receiver:** a single replica handles all webhooks. Each request reads
  and writes the K8s API and the GitHub API; both are external services
  and the receiver is naturally sharded by event delivery.
- **Runner:** N concurrent Jobs (one per PR). Cluster resources are the
  only ceiling. Typical resource request per Job: 1 CPU / 2 GiB memory.
- **GitHub App:** each Job mints its own installation token. Token cache
  is per-pod (so no cross-pod contention).

## Observability

- **Logs:** JSON to stdout (receiver via `slog`; runner via
  `console.log(JSON.stringify(...))`).
- **Events:** each run produces a `Job` object in the `dev-tools`
  namespace, with labels `app=boop`, `pr-number=<N>`, `sha=<full SHA>`
  and annotations `boop/owner`, `boop/repo`, `boop/number`, `boop/sha`,
  `boop/base-ref`. `kubectl get jobs -n dev-tools -l app=boop` lists
  everything in flight and in the last hour.
- **GitHub-side:** the status thread is the user-visible observability.
  Each stage appears as a one-liner; the final stage carries the
  pass/fail signal.
- **No metrics endpoint today.** Add a `/metrics` handler on the receiver
  if/when Prometheus scraping is needed.

## See also

- [webhook-contract.md](./webhook-contract.md) — accepted events, dedup, status thread.
- [deployment.md](./deployment.md) — how this all gets deployed.
- [secrets.md](./secrets.md) — credentials and where they live.
