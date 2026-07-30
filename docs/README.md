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
┌──────────────────┐  submit Job     ┌──────────────────┐
│  boop-receiver   │ ──────────────▶ │  boop-runner     │  one-shot K8s Job
│  (Go, 1 replica) │                 │  (Node, per PR)  │
│  - HMAC verify   │                 │  - clone PR      │
│  - filter events │                 │  - run boop skill│
│  - dedupe by SHA │                 │  - parse output  │
│  - post 👀       │                 │  - post summary  │
│  - submit Job    │                 │  - post inlines  │
└──────────────────┘                 └──────────────────┘
        │                                     │
        └─────── GitHub App creds (in-cluster secret) ───┐
                                                       ▼
                                          OpenRouter (LLM)
```

A reviewer (or the system) opens a PR. Within seconds BoopPr posts a 👀
"reviewing this PR…" comment. 1-3 minutes later, a summary comment with the
findings table arrives, followed by 0-8 line-specific inline comments pinned
to the diff. A final status update marks the run ✅ or ❌.

## Read order

1. **[product.md](./product.md)** — public perspective. What a PR author sees.
2. **[architecture.md](./architecture.md)** — components, data flow, request lifecycle.
3. **[webhook-contract.md](./webhook-contract.md)** — accepted events, dedup, status thread semantics.
4. **[receiver.md](./receiver.md)** — Go webhook receiver.
5. **[runner.md](./runner.md)** — Node PR-review worker.
6. **[skills.md](./skills.md)** — Boop review skill (orchestrator + seven lenses).
7. **[deployment.md](./deployment.md)** — K8s overlays, CI, image tags, ArgoCD.
8. **[secrets.md](./secrets.md)** — GitHub App credentials, OpenBao secret store.
9. **[development.md](./development.md)** — local dev, build, test.

The component READMEs (`apps/receiver/README.md`, `apps/runner/README.md`,
`apps/k8s/base/runner-config/README.md`) are scoped to that component and stay
authoritative for component-level details; the docs in this directory are the
cross-component, end-to-end view.

## Key concepts

- **Two services, one pipeline.** `boop-receiver` validates GitHub webhooks
  and submits K8s Jobs. `boop-runner` is a one-shot Job that does the review.
  No long-lived LLM process; every PR is a fresh pod.
- **Multi-lens review.** Seven lenses (code quality, design pattern, error
  handling, readability, SOLID, test quality, deep) applied in one model
  call. Findings are tier-prefixed and globally numbered (`B1`, `F2`, `O3`)
  so the author can write `fix B1` in a commit message and trace it.
- **Idempotency by head SHA.** Job name encodes the short SHA. Re-deliveries
  for the same head are no-ops (or, for `succeeded` Jobs, friendly
  "Already sniffed" replies); re-reviews on a new SHA get their own run.
- **Voice contract.** The summary and inline comments are what the author
  sees. The skill enforces a hard voice contract (ASD-STE-flavored, no
  slop, no marketing adjectives, no contractions, no emoji in finding
  bodies). See `apps/k8s/base/runner-config/skills/boop/SKILL.md`.
- **GitHub App auth.** The App's PEM private key + installation ID mint
  short-lived installation tokens. Token cache in both Go and Node
  services (`exp > now + 5min`).
- **Status thread.** The receiver pre-creates a "reviewing…" comment with
  the review label and trigger attribution. The runner PATCHes it at each
  stage (`auth` → `clone` → `review` → `done`/`failed`).

## Repository layout

```
.
├── README.md                  # this repo's front door
├── apps/
│   ├── receiver/              # Go: webhook receiver + Job submitter
│   │   ├── cmd/receiver/      # main entrypoint
│   │   └── internal/
│   │       ├── webhook/       # HTTP handler, Job template, dedup
│   │       └── github/        # GitHub App client (token mint, PR fetch)
│   ├── runner/                # Node: PR reviewer (boop skill runner)
│   │   └── src/               # index.mjs, review-header.mjs, tests
│   └── k8s/
│       ├── base/              # cluster-agnostic manifests
│       │   └── runner-config/ # OpenCode config + skills (ConfigMap)
│       └── overlays/
│           └── pugquilt/      # home cluster overlay
├── .github/workflows/         # build-receiver.yaml, build-runner.yaml
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
