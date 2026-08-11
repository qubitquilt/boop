# Deployment

How BoopPr gets built, published, and rolled out to clusters.

See also: [secrets.md](./secrets.md), [architecture.md](./architecture.md).

## Pipeline

```
push to main / v* tag
        │
        ▼
┌────────────────────────────┐
│  .github/workflows/        │
│    build-receiver.yaml     │  one workflow per image
│    build-runner.yaml       │
└──────────────┬─────────────┘
               │ docker buildx (kubernetes driver, buildx-builder ns)
               │ linux/arm64
               ▼
┌────────────────────────────┐
│  ghcr.io/qubitquilt/       │
│    boop-receiver:tag       │
│    boop-runner:tag         │
└──────────────┬─────────────┘
               │ workflow_run (on success)
               ▼
┌────────────────────────────┐
│  sync-image-digests.yaml   │  resolves :stable → sha256:...
│  opens a PR against main   │  human squash-merges
└──────────────┬─────────────┘
               │ git push to main
               ▼
┌────────────────────────────┐
│  ArgoCD Application        │  polls main, syncs the overlay
│  cluster repo             │
└──────────────┬─────────────┘
               │
               ▼
   dev-tools namespace
   ┌────────────┐
   │ Deployment │ boop-receiver
   │ Service    │ boop-receiver (ClusterIP)
   │ IngressR.  │ boop.qubitquilt.dev → /webhook
   │ ConfigMap  │ boop-config, boop-runner-config
   │ Secret     │ boop-secrets (synced from OpenBao)
   │ RBAC       │ Role, RoleBinding, ServiceAccounts
   └────────────┘
```

## Deployment lifecycle

End-to-end, a commit to `main` (or a `v*` tag) becomes a running pod.
Three systems are involved: GitHub Actions (build + push), a follow-up
GitHub Action (digest sync via PR), and ArgoCD (apply). Each owns one
stage of the chain.

### Workflow triggers

| Workflow | Trigger | Pushes image? | Runs when |
|---|---|---|---|
| `build-receiver` | `push` to `main` (scoped to `apps/receiver`) | yes | every commit that touches receiver code |
| `build-receiver` | `push` to `v*` tag | yes | each release tag |
| `build-receiver` | `pull_request` to `main` (scoped to `apps/receiver`) | no | PR builds; fork PRs skipped |
| `build-runner` | `push` to `main` (scoped to `apps/runner`) | yes | every commit that touches runner code |
| `build-runner` | `push` to `v*` tag | yes | each release tag |
| `build-runner` | `pull_request` to `main` (scoped to `apps/runner`) | no | PR builds; fork PRs skipped |
| `build-runner` | `workflow_dispatch` | yes | manual re-run from the Actions tab; also fired by the nightly schedule |
| `rebuild-runner-nightly` | `schedule` (daily 02:00 UTC) | no | calls `gh workflow run build-runner --ref main`, then `sync-image-digests` |
| `rebuild-runner-nightly` | `workflow_dispatch` | no | manual run of the same chain |
| `sync-image-digests` | `workflow_run` of either build, on `success` | n/a | runs after every successful build |
| `sync-image-digests` | `workflow_dispatch` | n/a | manual re-run from the Actions tab |

The two build workflows are independently scoped via `on.push.paths:`
(QUB-119 / PR #166):

- `build-receiver` matches `apps/receiver/**` and
  `.github/workflows/build-receiver.yaml`.
- `build-runner` matches `apps/runner/**` and
  `.github/workflows/build-runner.yaml`.

Editing a path outside both scopes (the kustomize overlay,
`sync-image-digests.yaml` itself, docs, root CI, etc.) produces no
build. The build step also has the guard:

```yaml
if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.fork == false
```

Fork PRs cannot use `packages: write`; the guard short-circuits the
build before `docker login` would fail.

### Sibling-wait short-circuit on single-subtree pushes (QUB-125)

When a push only touches one subtree, only the matching build fires.
`workflow_run` from that build still triggers `sync-image-digests`,
but the wait step would otherwise spin until the 5-minute deadline
polling for the sibling that the `paths:` filter just excluded. The
wait step now reads the files changed by the head SHA and skips the
poll when none of them match the sibling's `paths:` filter
(receiver-scope regex for the runner wait, runner-scope regex for the
receiver wait). The path filters are mirrored inline in the step; if
you change a `paths:` block in `build-receiver.yaml` /
`build-runner.yaml`, mirror it in `sync-image-digests.yaml`.

The same logic applies on `pull_request` events: a PR that only
touches runner code fires only `build-runner`, and the sync
short-circuits the wait for `build-receiver`.

Manual `workflow_dispatch` of `sync-image-digests` still skips the
wait entirely (`if: github.event_name == 'workflow_run'`).

### Main commit: build to rollout

1. Commit lands on `main` (direct push or PR merge).
2. The build workflows run conditionally on the commit's `paths:`
   filter. A commit that touches `apps/receiver/**` (or
   `.github/workflows/build-receiver.yaml`) fires `build-receiver`;
   a commit that touches `apps/runner/**` (or
   `.github/workflows/build-runner.yaml`) fires `build-runner`. A
   commit that touches both fires both. Each runs on the
   `boop-runner-set` Actions runner, calls
   `docker/metadata-action` for tags, builds `linux/arm64` with
   buildx, logs in to `ghcr.io`, and pushes on `push` events. On
   `pull_request` events, `push: ${{ github.event_name != 'pull_request' }}`
   is false; the build is local, no push.
3. On `success`, the `workflow_run` event fires
   `sync-image-digests`. The "wait for sibling upstream build"
   step skips its poll when the commit did not touch the
   sibling's subtree (QUB-125); it proceeds straight to the
   digest query.
4. `sync-image-digests` calls the GitHub packages API for the
   current `:stable` digest of each image, rewrites the digest in
   `apps/k8s/overlays/pugquilt/kustomization.yaml` (receiver) and in
   the `JOB_IMAGE` field of `apps/k8s/base/config.yaml` (runner).
5. The workflow opens a PR with the `skip-review` label and tries
   `--auto --squash`. If auto-merge is enabled, the PR merges
   itself. If not, a human squash-merges.
6. The merge commit lands on `main`. ArgoCD's Application is
   configured with `targetRevision: main` and polls every 3 minutes
   by default.
7. ArgoCD sees the digest change in the overlay, syncs the
   `boop-pugquilt` Application. The Deployment's pod template
   changes; K8s rolls pods. Old pods terminate; new pods pull the
   pinned digest and pass readiness probes.

Concurrency: `sync-image-digests` declares
`concurrency: { group: sync-image-digests, cancel-in-progress: true }`.
A second build finishing during an in-flight digest sync cancels
the first. The workflow also checks for an open digest PR before
opening a new one, so two simultaneous digest PRs cannot stack up.

### Release tag: from `vX.Y.Z` to running

1. Tag and push:
   ```
   git tag v0.2.0
   git push origin v0.2.0
   ```
2. The `push` event matches `tags: ['v*']`. Both build workflows
   run. `docker/metadata-action` produces:
   - `:stable`
   - `:v0.2.0` (from `type=semver,pattern=v{{version}}`)
   - `:0.2` (from `pattern={{major}}.{{minor}}`)
   - `:0` (from `pattern={{major}}`)
   - `:latest`, but only if `v0.2.0` is the highest semver seen
     (flavor `latest=auto`)

   Lowering `:latest` to a non-head commit never happens; `:latest`
   is always the most recent semver tag if one exists, otherwise the
   most recent `main` build.
3. The same `workflow_run` → `sync-image-digests` chain runs.
4. The same human merge and ArgoCD rollout.

To release without bumping `:latest` (e.g. a backport), tag it and
the digest sync still pins the overlay to the new bytes. `:latest`
keeps pointing at the previous highest semver until a higher one is
tagged.

### Why a PR for digest sync

The sync workflow could push directly to `main`. It opens a PR
instead so the repo's "changes must be made through a pull request"
branch protection rule is satisfied. The squash merge produces a
commit on `main` signed by the human merger, which satisfies the
`required_signatures` branch protection rule. The bot's
intermediate commit on the feature branch is not signed; debugging
bot-side SSH signing produced inconsistent signatures and was
abandoned in favor of this flow. The `skip-review` label bypasses
CODEOWNERS review; the human merger is the only required check.

### Rollback

Two paths, both end with ArgoCD syncing the new git state.

Pin a known-good digest directly:

```
# Resolve the digest for a known-good tag
DIGEST=$(docker buildx imagetools inspect ghcr.io/qubitquilt/boop-receiver:v0.1.0 --raw | jq -r .manifests[0].digest)

# Edit apps/k8s/overlays/pugquilt/kustomization.yaml:
#   - name: ghcr.io/qubitquilt/boop-receiver
#     digest: $DIGEST
git commit -am "pin receiver to v0.1.0"
git push
# ArgoCD syncs
```

Pin the runner:

```
# Edit apps/k8s/base/config.yaml JOB_IMAGE to point at the desired tag
git commit -am "bump runner to 0.1.0"
git push
# ArgoCD syncs; next Job pulls the pinned image
```

The receiver rolls on the next sync. The runner is consumed on the
next Job submit (Jobs are not owned by ArgoCD, so there is nothing
to roll).

### JOB_IMAGE runtime resolution

The receiver reads `JOB_IMAGE` from the `boop-config` ConfigMap on
**every webhook submit**, not at startup. The Pod's `JOB_IMAGE`
env var is still wired through `configMapKeyRef`, but only as a
fallback for when the K8s API read fails. This avoids the
"ArgoCD syncs the ConfigMap but the receiver pod doesn't restart,
so the next Job pulls the old image" race that bit us on
2026-08-01 (PR #83).

The receiver's RBAC Role grants `configmaps:get,list,watch` in
`TARGET_NAMESPACE` only — `apps/k8s/base/role.yaml`. Reads are
namespace-scoped to `dev-tools`, so a compromised receiver cannot
enumerate ConfigMaps cluster-wide.

Cost: one extra `Get` per webhook. Webhooks are bounded by GitHub's
delivery rate (one per push per repo, in practice), and the read
returns from the API server's in-memory cache, so the latency
overhead is single-digit milliseconds.

### Self-heal for the runner image (QUB-122)

The push trigger on `build-runner.yaml` is scoped to
`apps/runner/**` and `.github/workflows/build-runner.yaml`. A fix
to anything outside that path (the deploy workflow, a kustomize
overlay, a sibling app, a docs page) leaves the runner image
behind the source until the next `apps/runner/**` change. The
ConfigMap `JOB_IMAGE` digest is a one-step-behind target — once
`:stable` lands, nothing forces a rebuild even if the source tree
has moved on.

`rebuild-runner-nightly.yaml` closes the gap. A daily cron at
02:00 UTC calls `gh workflow run build-runner --ref main`, waits
for it to complete, then calls `gh workflow run sync-image-digests`.
The build is cache-warmed (buildx layer cache; the runner image
spends ~25s on a no-op rebuild) so a no-op rebuild completes in
seconds, and `sync-image-digests` produces no diff when the digest
has not changed (`git diff --quiet` short-circuits the PR step).

The explicit `sync-image-digests` dispatch is required to skip its
"wait for sibling upstream" step. The step polls for
`build-receiver`'s successful run on the same head SHA — a guard
that prevents half-correct digest pairs when the two builds
finish at different latencies. A nightly rebuild that only triggers
`build-runner` has no `build-receiver` run for the current SHA, so
the wait would time out after 5 minutes (or, with QUB-125,
short-circuit only if the current `main` tip commit happens to
not touch any `apps/receiver/**` file — a property the rebuild
cannot rely on across days). A direct `workflow_dispatch` of
`sync-image-digests` skips the step
(`if: github.event_name == 'workflow_run'`), and the workflow's
`concurrency.cancel-in-progress` group coalesces the two runs
into one — the failed `workflow_run` is cancelled the moment the
explicit dispatch lands.

Manual triggers:

- `Actions → rebuild-runner-nightly → Run workflow` for an
  end-to-end rebuild + digest sync on demand.
- `Actions → build-runner → Run workflow` for a rebuild without
  forcing the digest sync (useful when you only want to push
  bytes; the digest sync will fire from `workflow_run` and will
  correctly pin whatever is on `:stable` once both upstreams
  complete on the same SHA).

## Image publishing

Two GitHub Actions workflows, one per image. Each runs on the
`boop-runner-set` Actions runner (Kubernetes driver, `buildx-builder`
namespace, sticky load balance) and builds `linux/arm64` only. See
[Deployment lifecycle](#deployment-lifecycle) for the full trigger
table and the order of operations.

Tags produced (`boop-receiver` and `boop-runner`):

| Trigger | Tags |
|---|---|
| push to `main` | `latest` |
| tag `vX.Y.Z` | `stable`, `vX.Y.Z`, `X.Y`, `X` (plus `latest` if it's the highest semver) |

Both `boop-receiver` and `boop-runner` share the same tag scheme so an
operator can pin the runner image independently of the receiver by
editing `JOB_IMAGE` in the `boop-config` ConfigMap.

### Digest pinning

The default overlay does **not** track `:latest` or `:stable` directly.
Instead, each image is pinned to a SHA digest:

```yaml
images:
  - name: ghcr.io/qubitquilt/boop-receiver
    digest: sha256:…
  - name: ghcr.io/qubitquilt/boop-runner
    digest: sha256:…
```

The `sync-image-digests` workflow (`.github/workflows/sync-image-digests.yaml`)
runs after every successful `build-receiver` or `build-runner` run, queries
GHCR for the current `:stable` digest of each image, and updates the
overlay. This is what makes the K8s Deployment roll on a registry-only
image change: ArgoCD tracks the git manifest, the manifest changes, K8s
sees a diff in the pod template, the Deployment rolls.

To pin a specific tag (e.g. for a hotfix rollback or a multi-cluster
pin to a known-good build), edit the overlay by hand:

```bash
# edit apps/k8s/overlays/pugquilt/kustomization.yaml to set the digest
# of the receiver or runner to the value of `docker buildx imagetools
# inspect <image>:<tag> --raw | jq -r .manifests[0].digest`
```

Or trigger `sync-image-digests` from the Actions tab with
`workflow_dispatch` — it will resolve the digest of whichever tag the
GHCR `:stable` currently points to.

## Kustomize layout

```
apps/k8s/
├── base/                          cluster-agnostic
│   ├── namespace.yaml             Namespace: dev-tools
│   ├── serviceaccount.yaml        SA: boop-receiver, boop-job
│   ├── role.yaml                  Role + RoleBinding for boop-receiver
│   ├── deployment.yaml            Deployment + Service for boop-receiver
│   ├── config.yaml                ConfigMap: boop-config
│   ├── kustomization.yaml         configMapGenerator, resources
│   ├── job-template.yaml          LEGACY: superseded by the embedded one
│   └── runner-config/             packed into boop-runner-config ConfigMap
│       └── skills/boop/...
└── overlays/
    └── pugquilt/                  home cluster
        ├── kustomization.yaml     namespace: dev-tools, image tags
        └── ingressroute.yaml      Traefik IngressRoute
```

### `base/`

- **Namespace:** `dev-tools`.
- **ServiceAccounts:** `boop-receiver` (long-lived) and `boop-job`
  (per-Job pod). Both reference `imagePullSecrets: ghcr-pull`.
- **Role:** `boop-receiver` can `get/list/watch/create/delete` `jobs`
  in the `batch` API group, and `get/list` `pods` + `pods/log` in the
  core group. Enough to dedupe by Job name and to read pod logs for
  debugging. The `boop-job` SA needs no extra RBAC (the runner does
  not call the K8s API).
- **Deployment:** 1 replica, ports `8080/http`, env from
  `boop-config` and `boop-secrets`. Liveness + readiness probes on
  `GET /health` (initial delay 5 s / 2 s, period 10 s / 5 s).
  Resources: 50 m / 64 MiB → 200 m / 128 MiB.
- **Service:** `boop-receiver` ClusterIP on port 8080.
- **ConfigMap `boop-config`:** `LOG_LEVEL`, `PORT`, `JOB_IMAGE`,
  `TARGET_NAMESPACE`. Plain text values; nothing secret here.

### `overlays/pugquilt/`

The home cluster. Pins to image digests (updated by the
`sync-image-digests` workflow on every push). For reproducible rollouts,
pin a specific digest by hand. Sets `namespace: dev-tools` and adds the
`cluster: pugquilt` label (non-selector). Ingress: Traefik
`IngressRoute` for `Host(boop.qubitquilt.dev) && PathPrefix(/webhook)`
→ `boop-receiver:http` on `websecure`.

To add a new cluster:

1. `mkdir -p apps/k8s/overlays/<cluster-name>`
2. Copy `overlays/pugquilt/kustomization.yaml` and adjust the image
   tags and label.
3. Copy `overlays/pugquilt/ingressroute.yaml` and adjust the host and
   TLS.
4. Add an ArgoCD Application in the cluster repo pointing at the
   overlay path.

## GitHub App setup

The App lives in the `qubitquilt` org. Required configuration:

- **Webhook URL:** `https://boop.qubitquilt.dev/webhook` (or whichever
  cluster the App routes through).
- **Webhook secret:** matches `WEBHOOK_SECRET` in `boop-secrets`.
- **Permissions:**
  - `Pull requests`: read & write (post comments, post review
    comments, react).
  - `Issues`: read & write (the status thread is an issue comment).
  - `Contents`: read (clone).
  - `Metadata`: read (default).
- **Events:** `Pull request`, `Issue comment`.
- **Installation:** on the `qubitquilt` org. The `installation_id` goes
  into `GITHUB_APP_INSTALLATION_ID`.

The App's PEM private key (`GITHUB_APP_PRIVATE_KEY`) is stored in
OpenBao at `secret/dev-tools/boop`.

See [secrets.md](./secrets.md) for the full credential list and the
OpenBao wiring.

## ArgoCD wiring

ArgoCD Applications live in the cluster repo (`qubitquilt/homelab-infra`
or similar). One Application per cluster:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: boop-pugquilt
  namespace: argocd
spec:
  project: dev-tools
  source:
    repoURL: https://github.com/qubitquilt/boop.git
    targetRevision: main
    path: apps/k8s/overlays/pugquilt
  destination:
    server: https://kubernetes.default.svc
    namespace: dev-tools
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=false
```

The Application owns: the `boop-receiver` Deployment + Service, the
`boop-config` ConfigMap, the `boop-runner-config` ConfigMap, the
ServiceAccounts + Role, the Traefik IngressRoute, and the
`boop-secrets` sync from OpenBao. The Job is **not** owned by ArgoCD
— the receiver creates it on demand.

## Rollout

The full automated flow is documented in
[Deployment lifecycle](#deployment-lifecycle). This section covers the
manual paths: pushing a custom build and pinning by digest.

### Pushing a custom build

Build and push from a checkout:

```
cd apps/receiver
make docker-build docker-push IMAGE=ghcr.io/qubitquilt/boop-receiver:0.2.0
cd ../runner
make docker-build docker-push IMAGE=ghcr.io/qubitquilt/boop-runner:0.2.0
```

Then either wait for the next digest-sync workflow run, or trigger
`sync-image-digests` from the Actions tab to pick up the new tags
immediately. To pin the runner without re-tagging, edit
`apps/k8s/base/config.yaml` `JOB_IMAGE` to the new tag and push.

### Pinning a specific digest

```
DIGEST=$(docker buildx imagetools inspect ghcr.io/qubitquilt/boop-receiver:v0.2.0 --raw | jq -r .manifests[0].digest)
# edit apps/k8s/overlays/pugquilt/kustomization.yaml:
#   - name: ghcr.io/qubitquilt/boop-receiver
#     digest: $DIGEST
git commit -am "pin receiver to v0.2.0"
git push   # ArgoCD syncs
```

The receiver is restarted by the Deployment rollout; the runner image
is consumed on the next Job submit. No coordination required.

## Image layout

| Image | Base | Size | Why |
|---|---|---|---|
| `boop-receiver` | `gcr.io/distroless/static-debian12:nonroot` | small (~15 MB) | Static, no shell, no package manager, non-root. |
| `boop-runner` | `ubuntu:24.04` + Node 22 + `@openrouter/agent` + `rtk` 0.44.2 | medium (~275 MB) | The Agent SDK + Client SDK transitively (TypeScript types, the Node 22 stdlib). Ubuntu 24.04's glibc 2.39 is the floor; Debian 12's glibc 2.36 is too old. rtk adds ~25 MB and runs from a per-arch prebuilt tarball pulled at build time with a pinned SHA-256. |

Both are built `linux/arm64` only. The cluster nodes are arm64; the
`boop-runner-set` Actions runners are arm64.

The runner image bakes the rtk binary, its config.toml, the
boop `filters.toml`, and the `rtk trust --yes` trust store at
build time so the first file read in the pod does not hit an
interactive prompt. Telemetry is off at three layers (config,
baked env, per-call env forwarded by the adapter). See
[`runner.md`](./runner.md#rtk-adapter-qub-85) for the adapter
contract.

## Local K8s smoke test

```
# Receiver: run locally, point at a real cluster
export WEBHOOK_SECRET=$(openssl rand -hex 32)
export GITHUB_APP_ID=...
export GITHUB_APP_INSTALLATION_ID=...
export GITHUB_APP_PRIVATE_KEY="$(cat /path/to/key.pem)"
export KUBECONFIG=~/.kube/config
cd apps/receiver
make run   # ./bin/receiver on :8080

# In another terminal, forward smee.io → localhost:8080
smee --url https://smee.io/<channel> --target http://localhost:8080/webhook

# Then point the GitHub App webhook at the smee URL and open a PR.
```

## See also

- [secrets.md](./secrets.md) — credentials and OpenBao wiring.
- [architecture.md](./architecture.md) — runtime topology.
- [development.md](./development.md) — build / test commands.
