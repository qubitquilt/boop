# BoopPr — Documentation

> Internal project: `boop`. GitHub App on the wire: **BoopPr**. Same pug, different name tag.

A GitHub App that reviews pull requests across the `qubitquilt` org: multi-lens
review, line-specific inline comments, one summary per PR, friendly status
updates while the review is in flight.

## The one-screen overview

```
GitHub App "BoopPr"
        │ webhook (pull_request, issue_comment)
        ▼
┌──────────────────┐  submit Job     ┌────────────────────────────────┐
│  boop-receiver   │ ──────────────▶ │  boop-runner                   │  one-shot K8s Job
│  (Go, 1 replica) │                 │  (Node, per PR)                │
│  - HMAC verify   │                 │  - clone PR                    │
│  - filter events │                 │  - walkthrough (1 LLM)         │
│  - dedupe by SHA │                 │  - classify   (1 LLM)          │
│  - post 🐾       │                 │  - N experts in parallel       │
│  - react 👀      │                 │  - gather / meta-review (1 LLM)│
│  - submit Job    │                 │  - narrate     (1 LLM)         │
│  - SQLite data   │ ◀── telemetry ──│  - ste-lint (mechanical)       │
│    layer         │     + status    │  - parse output                │
└──────────────────┘                 │  - post summary + inlines      │
        │                            │  - 🦴 done / ❌ failed         │
        │                            └────────────────────────────────┘
        │                                     │
        └─────── GitHub App creds (in-cluster secret) ──┐
                                                       ▼
                                          OpenRouter (LLM)
```

A reviewer (or the system) opens a PR. Within seconds BoopPr posts a 🐾
"Boop's on the case!" status comment (or, for comment-triggered
re-reviews, react 👀 on the trigger). 1-3 minutes later, a summary
comment with the findings table arrives, followed by 0-8 line-specific
inline comments pinned to the diff. Final state: 🦴 (bone) on the
status comment + a `Reviewed by BoopPr` line, or ❌ on failure. In
reaction mode the 🐾 comment never appears and the 👀 becomes 🦴 or ❌.

## Read order

1. **[product.md](./product.md)** — public perspective. What a PR author sees.
2. **[architecture.md](./architecture.md)** — components, data flow, request lifecycle.
3. **[webhook-contract.md](./webhook-contract.md)** — accepted events, dedup, status thread semantics.
4. **[receiver.md](./receiver.md)** — Go webhook receiver.
5. **[runner.md](./runner.md)** — Node PR-review worker.
6. **[skills.md](./skills.md)** — Boop review skill (orchestrator + seven lenses).
7. **[deployment.md](./deployment.md)** — K8s overlays, CI, image tags, ArgoCD, release lifecycle.
8. **[secrets.md](./secrets.md)** — GitHub App credentials, OpenBao secret store.
9. **[development.md](./development.md)** — local dev, build, test.
10. **[workflow-engine.md](./workflow-engine.md)** — engine choice for the staged PR review (QUB-87).

The component READMEs (`apps/receiver/README.md`, `apps/runner/README.md`,
`apps/k8s/base/runner-config/README.md`) are scoped to that component and stay
authoritative for component-level details; the docs in this directory are the
cross-component, end-to-end view.

## Key concepts

- **Two services, one pipeline.** `boop-receiver` validates GitHub webhooks
  and submits K8s Jobs. `boop-runner` is a one-shot Job that does the review.
  No long-lived LLM process; every PR is a fresh pod.
- **Multi-expert review.** One walkthrough call generates a
  human-readable PR summary. N parallel expert calls
  (one per lens; today seven) generate findings, each
  with the lens file as system prompt and the walkthrough
  plus diff as user message. A meta-reviewer pass can
  request a bounded re-pass of experts that produced
  findings that "stick out as potentially wrong" (one
  re-pass per run, no loops). A narrator synthesizes the
  walkthrough + gathered findings into the final review.
  The output shape is the same `=== SUMMARY === …
  === INLINE COMMENTS === … === END ===` block as the
  single-call path, so the runner's post-parse surface is
  unchanged.
- **Idempotency by head SHA.** Job name encodes the short SHA. Re-deliveries
  for the same head are no-ops (or, for `succeeded` Jobs, friendly
  "Already sniffed" replies); re-reviews on a new SHA get their own run.
- **Persona.** A curated pool of light pug flourishes
  (`resources/persona.md`) that the narrator samples from
  once per review. Used in the summary TL;DR opener, the
  "What this PR does well" opener, and the line after the
  closing `Approving | Changes requested | Commented`
  token. Never in inline comment bodies.
- **Voice contract.** The summary and inline comments are what the author
  sees. The skill enforces a hard voice contract (ASD-STE-flavored, no
  slop, no marketing adjectives, no contractions, no emoji in finding
  bodies). The runner's `lib/ste-lint.mjs` runs the same checks
  mechanically before posting (best-effort; LLM is the source of
  truth). See `apps/k8s/base/runner-config/skills/boop/SKILL.md`.
- **GitHub App auth.** The App's PEM private key + installation ID mint
  short-lived installation tokens. Token cache in both Go and Node
  services (`exp > now + 5min`).
- **Status thread (PR-opened).** The receiver pre-creates a
  "reviewing…" comment with the review label and trigger
  attribution. The runner PATCHes it at each stage
  (`auth` → `clone` → `review` → `done`/`failed`).
- **Reaction mode (PR-comment).** When triggered by
  `@BoopPr review` on a comment, the receiver reacts 👀
  on the trigger and sets `BOOP_NO_STATUS_COMMENT=1` on
  the Job. The runner does not post or PATCH a status
  comment; it adds a single terminal reaction (🦴 on
  done, ❌ on failed) on the trigger comment. One
  reaction change, one notification.
- **rtk adapter (QUB-85).** All file reads the runner
  does (the SKILL.md, the lens files, the persona
  resource) route through `lib/rtk.mjs`, which shells
  out to the `rtk` CLI for compression and falls back
  to raw `fs.readFile` when rtk is missing or
  `BOOP_RTK_DISABLED=1`.
- **Data layer (QUB-101).** SQLite on a PVC backs the
  dashboard's `/api/reviews`, `/api/runs`,
  `/api/installations`, `/api/stats` endpoints and the
  runner's telemetry / status POSTs. The webhook path
  is unaffected by the data layer being disabled
  (`DB_PATH` empty / unopenable).

## Repository layout

```
.
├── README.md                  # this repo's front door
├── apps/
│   ├── receiver/              # Go: webhook receiver + Job submitter
│   │   ├── cmd/receiver/      # main entrypoint
│   │   └── internal/
│   │       ├── webhook/       # HTTP handler, Job template, dedup
│   │       │   #   + dashboard.go (SQLite read endpoints),
│   │       │   #   + reviews.go (legacy Job-grouped read),
│   │       │   #   + jobbuilder.go, retry-on-orphan fix
│   │       ├── store/         # SQLite data layer, retention, backup,
│   │       │   #   migrations, telemetry/stats aggregations
│   │       └── github/        # GitHub App client (token mint, PR fetch)
│   ├── runner/                # Node: PR reviewer (boop skill runner)
│   │   ├── src/               # index.mjs, review-header.mjs, tests
│   │   │   #   lib/config, log, security, git, github, dashboard,
│   │   │   #   classify, walkthrough, experts, ste-lint, rtk, workflow
│   │   ├── rtk/               # rtk config.toml + filters.toml (baked)
│   │   └── Dockerfile         # ubuntu 24.04 + node 22 + rtk 0.44.2
│   └── k8s/
│       ├── base/              # cluster-agnostic manifests
│       │   └── runner-config/ # OpenCode config + skills (ConfigMap)
│       │       #   skills/boop/{SKILL.md, agents/*.md, resources/*.md}
│       └── overlays/
│           └── pugquilt/      # home cluster overlay
├── .github/workflows/         # build-receiver.yaml, build-runner.yaml,
│                              #   sync-image-digests.yaml
└── docs/                      # this directory
```

## Naming

| Wire / product | Repo | Source of truth |
|---|---|---|
| GitHub App name | `BoopPr` | GitHub App settings |
| Bot login | `BoopPr[bot]` | `BOT_LOGIN` env / `deployment.yaml` |
| Internal project | `boop` | `apps/receiver`, `apps/runner` |
| Go module | `github.com/michaelruelas/boop-receiver` | `apps/receiver/go.mod` |
| Receiver image | `ghcr.io/qubitquilt/boop-receiver` | `apps/k8s/base/deployment.yaml` |
| Runner image | `ghcr.io/qubitquilt/boop-runner` | `apps/k8s/base/job-template.yaml` |
