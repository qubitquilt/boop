# Architecture

End-to-end view of how a webhook becomes a PR review.

## Topology

```
                                   ┌─────────────────┐
                                   │  OpenRouter LLM │
                                   └────────▲────────┘
                                            │ HTTPS (parallel + serial calls)
                                            │
   GitHub (PR / comment)                    │
        │ webhook                           │
        ▼                                   │
┌──────────────────┐  submit Job    ┌───────┴──────────────────────────────┐
│  boop-receiver   │ ──────────────▶│  boop-runner                         │
│  (Deployment,    │                │  (Job, 1 pod / PR)                   │
│   1 replica,     │                │                                       │
│   in-cluster)    │ ◀── telemetry  │  - clone PR         [fetch]          │
│                  │      status    │  - walkthrough      [1 SDK call]     │
│  - HMAC verify   │                │  - classify         [1 SDK call]     │
│  - filter events │                │  - N experts        [N parallel]     │
│  - dedupe by SHA │                │  - gather           [in-process]     │
│  - post 🐾       │                │  - meta-review      [1 SDK call]     │
│  - react 👀      │                │  - narrate          [1 SDK call]     │
│  - mint comments │                │  - ste-lint         [mechanical]     │
│  - data layer    │                │  - post summary                      │
│   (SQLite +      │                │  - post inlines                      │
│    PVC)          │                │  - post final reaction (QUB-114)     │
└────────▲─────────┘                └───────▲──────────────────────────────┘
         │                                  │
         │     ┌────────────────────────┐   │
         └──── │  Kubernetes API server │ ──┘
               │  (in-cluster)          │
               └────────────────────────┘
```

Two services, one pipeline. The receiver is a long-lived HTTP server. The
runner is a one-shot Kubernetes Job per PR. The runner's N-expert
pipeline (one walkthrough + N parallel experts + gather + meta-review +
narrate) spans the LLM box; the receiver and the K8s API sit between
GitHub and the runner.

## Components

### boop-receiver (`apps/receiver/`)

Long-lived. Validates HMAC, filters events, dedupes by head SHA, posts the
"reviewing…" status comment, submits a Job. Stateless across requests —
all state lives in the K8s API and the GitHub API.

- **Image:** `ghcr.io/qubitquilt/boop-receiver` (Go 1.23, distroless).
- **Replicas:** 1 (eventually consistent — duplicates are deduped by head
  SHA, not by receiver instance).
- **Endpoints:** `POST /webhook`, `GET /health`, `GET /api/reviews` (the
  latter for dashboards; see [receiver.md](./receiver.md#apireviews));
  plus the data-layer endpoints `GET /api/installations`,
  `GET /api/runs`, `GET /api/stats`, and the runner POST endpoints
  `POST /api/runs/{id}/telemetry`, `POST /api/runs/{id}/status`.
- **Cluster scope:** namespace `dev-tools`. Creates Jobs in the same
  namespace.
- **GitHub App auth:** uses the App creds to post the "reviewing…"
  comment and to fetch PR metadata for `@BoopPr review` triggers.
- **Data layer:** a SQLite file on a PVC backs the data-layer
  endpoints. The receiver writes the run row on Job submit; the
  runner posts the telemetry + status updates as the run
  progresses. The data layer is opt-in: a missing or unopenable
  `DB_PATH` returns 503 on the new endpoints without affecting
  the webhook path or `/api/reviews`.

See [receiver.md](./receiver.md).

### boop-runner (`apps/runner/`)

Short-lived. One pod per PR review. Clones the PR, runs the
multi-expert review pipeline via the OpenRouter Agent SDK
in-process, posts the result.

- **Image:** `ghcr.io/qubitquilt/boop-runner` (Ubuntu 24.04, Node 22,
  `@openrouter/agent` from npm, `rtk` 0.44.2 binary). The image is
  ~250 MB; rtk adds ~25 MB.
- **Lifetime:** 1 pod, started by the receiver, runs to completion or 30
  min (`activeDeadlineSeconds: 1800`), GC'd 1 h after finish.
- **Workspace:** `/work/repo` (the cloned PR). The skill ConfigMap
  mounts read-only at `/home/opencode/.config/opencode`; the runner
  reads skill files directly from the mount, going through the
  [rtk adapter](#file-reads-rtk-adapter-qub-85) when rtk is present.
- **No outbound network** except: GitHub (clone + API), OpenRouter (LLM).
- **No subprocess for the LLM.** The SDK call is in-process; the
  runner does not shell out to an LLM CLI. (`rtk read` is the one
  subprocess left, see [rtk adapter](#file-reads-rtk-adapter-qub-85).)

#### Pipeline shape (multi-expert, QUB-95 + QUB-114)

The runner's `lib/workflow.ts` walks six macro stages:
**handshake → fetch → sniff → summary → inlines → cleanup**. The
`sniff` stage wraps a six-step sub-workflow (`REVIEW_SUB_STAGES`):

1. **walkthrough** — one LLM call reads the diff and produces a
   10-20 sentence human-readable summary. Every expert reads it
   as shared context. (Failure is non-fatal: experts can fall back
   to reading the diff directly.)
2. **classify** — one LLM call tags the PR type
   (`bug fix | feature | refactor | docs | test-only | infra`).
3. **dispatch** — N parallel LLM calls, one per active lens, with
   the lens file as system prompt and walkthrough + diff as user
   message. Each expert returns `{ findings: Finding[] }`.
4. **gather** — de-duplicates and flattens the expert findings.
5. **meta-review** — scans the gathered findings for things that
   "stick out as potentially wrong" and, if any, requests a
   bounded re-pass of the specific experts that produced them.
   Bounded to one re-pass per run (no loops).
6. **narrate** — one final LLM call synthesizes the walkthrough
   and gathered findings into the structured
   `=== SUMMARY === / === INLINE COMMENTS === / === END ===`
   block the runner parses.

The narrator picks up the persona
(`apps/k8s/base/runner-config/skills/boop/resources/persona.md`) —
one light pug flourish per review, in the TL;DR opener, "What
this PR does well" opener, or the line after the closing
`Approving | Changes requested | Commented` token. Never in inline
comment bodies.

After parse, the runner's `lib/ste-lint.ts` runs the same
STE-flavored checks the skill mandates (no contractions, no
marketing adjectives, ≤20-word sentences, no emoji in bodies) on
the LLM output before posting. The linter is mechanical and
best-effort — drift is logged and not re-fed to the LLM; the
LLM is the source of truth.

See [runner.md](./runner.md).

### File reads: rtk adapter (QUB-85)

Every file read the runner does — the orchestrator
(`SKILL.md`), the seven lens files
(`agents/review-*.md`), and the persona resource
(`resources/persona.md`) — routes through `lib/rtk.ts`. The
adapter shells out to `rtk read` for compression; when rtk is
missing or `BOOP_RTK_DISABLED=1` is set, the adapter falls back
to raw `fs.readFile` transparently. The adapter is the single
place rtk is invoked.

The image bakes the rtk binary (`/usr/local/bin/rtk`), the rtk
config (`/home/opencode/.config/rtk/config.toml`), the boop
filter bundle (`filters.toml`), and the trust store populated
at build time (`rtk trust --yes`). Telemetry is off at three
layers (config, baked env, per-call env). The custom filter
caps lens file reads at 400 lines / 500 chars/line so a
runaway file cannot blow up the prompt context.

See [runner.md](./runner.md#rtk-adapter-qub-85).

### Skills & config ConfigMap

`apps/k8s/base/runner-config/` packs into the `boop-runner-config`
ConfigMap. Contents:

| Key | Path | Purpose |
|---|---|---|
| `skill-boop` | `…/skills/boop/SKILL.md` | orchestrator |
| `skill-boop-agent-code-quality` | `…/skills/boop/agents/review-code-quality.md` | lens 1 |
| `skill-boop-agent-design-pattern` | `…/skills/boop/agents/review-design-pattern.md` | lens 2 |
| `skill-boop-agent-error-handling` | `…/skills/boop/agents/review-error-handling.md` | lens 3 |
| `skill-boop-agent-readability` | `…/skills/boop/agents/review-readability.md` | lens 4 |
| `skill-boop-agent-solid-principles` | `…/skills/boop/agents/review-solid-principles.md` | lens 5 |
| `skill-boop-agent-test-quality` | `…/skills/boop/agents/review-test-quality.md` | lens 6 |
| `skill-boop-agent-deep` | `…/skills/boop/agents/review-deep.md` | lens 7 |
| `skill-boop-resource-persona` | `…/skills/boop/resources/persona.md` | curated persona pool (QUB-114) |
| `skill-boop-resource-lens-template` | `…/skills/boop/resources/lens-template.md` | shared lens-template format |
| `skill-boop-resource-output-format` | `…/skills/boop/resources/output-format.md` | the exact output block the narrator must emit |

The model name comes from the `OPENROUTER_MODEL` Job env var (set
by the receiver from the cluster default). QUB-98 removed the
`opencode.json` ConfigMap key; the model is no longer sourced
from the ConfigMap.

ConfigMap limit: 1 MB (etcd hard cap). The current payload is well under.
Bumping against the limit would require moving to a git-sync init
container pattern.

See [skills.md](./skills.md).

## Request lifecycle

The PR-opened path (status-thread surface) and the comment-triggered
path (reaction surface, QUB-114) share the underlying pipeline;
they differ in the user-visible surface only.

### PR-opened path (status-thread surface)

```
T+0s   GitHub delivers a pull_request webhook to /webhook
T+0s   Receiver verifies X-Hub-Signature-256 (HMAC-SHA256, constant-time)
T+0s   Receiver parses, filters, dedupes by Job name (= head SHA)
T+0s   Receiver posts 🐾 status comment (no trigger attribution;
       no label change for first review)
T+0s   Receiver renders Job template, submits Job
T+1s   Job pod starts; runner mints installation token
T+1s   runner PATCHes status → 🤝 paw-shaken in
T+2s   runner clones PR (`git clone --depth 50` + `fetch --depth 200`)
T+5s   runner PATCHes status → 🥎 fetched
T+5s   runner PATCHes status → 👃 sniffing (sniff macro-stage starts)
T+5s     walkthrough LLM call (1 SDK call)
T+5s     classify LLM call (1 SDK call)
T+5s     N expert LLM calls in parallel (7 today)
T+?s       meta-review may re-pass up to N experts (bounded once)
T+?s     narrate LLM call (1 SDK call) → structured block
T+?s     ste-lint runs mechanically, logs drift
T+60s..120s   runner parses the structured block
T+60s..120s   runner posts summary comment + 0-8 inline comments
T+60s..120s   runner PATCHes status → 🦴 bone delivered
T+fail   on any error: PATCH status → ❌ lost the bone
```

### Comment-triggered path (reaction surface, QUB-114)

```
T+0s   GitHub delivers an issue_comment webhook to /webhook
T+0s   Receiver verifies, parses, drops self-mentions and reference
       mentions; only `@BoopPr review` (per the request grammar)
       submits a Job
T+0s   Receiver dedupes by Job name; if a Job for the same head
       already succeeded, post "Already sniffed" and ack duplicate
T+0s   Receiver reacts 👀 on the trigger comment (NOT a 🐾 post)
T+0s   Receiver renders Job template with BOOP_NO_STATUS_COMMENT=1,
       submits Job
T+1s   runner mints installation token; postStatus wrapper is
       a no-op (no octokit yet, no status comment id)
T+?s   runner clones PR
T+?s   runner runs the same multi-expert pipeline
T+?s   runner posts summary + inline comments to the PR
T+?s   runner adds a single terminal reaction on the trigger
       comment: 🎉 on done, 👎 on failed
```

The author's view on a comment-triggered re-review is a one-step
transition: 👀 → 🎉 (or 👎). One reaction change, one notification,
no PATCH loop.

### Total wall-clock

1-3 minutes for a typical PR. Dominated by the LLM calls.
- One walkthrough + one classify + N expert calls (N=7 today,
  in parallel) + one narrate is the LLM budget per review.
  Meta-review adds at most one more pass through the experts it
  flags, never a loop.
- The SDK call has a 25-min hard ceiling; the Job's
  `activeDeadlineSeconds` is 30 min, leaving 5 min of headroom for
  post-review work.

## State

State lives in four places:

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
   is minted and exchanged. The receiver also caches the App
   installation list (5 min) so the dashboard's poll does not hammer
   GitHub. No persistent local state.
4. **SQLite (data layer).** A single file on a PVC stores every run
   and its telemetry. The receiver writes the run row on Job submit;
   the runner POSTs the final telemetry at the end of the review. The
   data layer is the source of truth for runs older than the Job TTL
   (1h after finish) and for all the dashboard's aggregations.

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
| LLM times out | `postStatus("failed")` (or terminal ❌ reaction in reaction mode), Job exits non-zero | The 25-min hard ceiling means a hung call is killed; the failure is visible in the status thread / on the trigger comment |
| LLM returns empty / unparseable output | Status gate rejects with `summary parse failed: <reason>` | Same; the runner exits 1, the pod is GC'd by the TTL |
| ConfigMap missing / unreadable | Status thread sees a `failed` with `lens … read attempt N failed` detail | The skill body is read with retry + 1s backoff to absorb transient `..data` symlink races right after pod start. Reads go through the rtk adapter, which falls back to `fs.readFile` if rtk is missing |
| Orphan status comment between postStatus and createJob (QUB-99) | The receiver pre-creates the status comment only after Job submit succeeds. A failed Job submit on the FIRST event does not leave an orphan 🐾; a redelivery posts one. | Re-deliver the event; the receiver now reports `queued` in the ack on the path where the Job submit is still in flight |
| Duplicate review from a sibling runner pod (QUB-125) | The second pod sees `state.passed` from the first pod via the workflow-state comment marker and aborts as `parseFailed` | No operator action; the abort path is the deduplication belt-and-suspenders for K8s scheduling races |

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
