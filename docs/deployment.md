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
               │
               │ (ArgoCD watches the cluster repo)
               ▼
┌────────────────────────────┐
│  ArgoCD Application        │  points at apps/k8s/overlays/<cluster>
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

## Image publishing

Two GitHub Actions workflows, one per image. Each:

- Triggers on `push` to `main` or any `v*` tag, and on `pull_request` to
  `main`.
- Runs on the `boop-runner-set` Actions runner (Kubernetes driver,
  buildx-builder namespace, sticky load balance).
- Builds `linux/arm64` only.
- On `push` events, computes tags via `docker/metadata-action` and
  pushes; on `pull_request` events, builds without pushing.

Tags produced (`boop-receiver` and `boop-runner`):

| Trigger | Tags |
|---|---|
| push to `main` | `latest` |
| tag `vX.Y.Z` | `stable`, `vX.Y.Z`, `X.Y`, `X` (plus `latest` if it's the highest semver) |

Both `boop-receiver` and `boop-runner` share the same tag scheme so an
operator can pin the runner image independently of the receiver by
editing `JOB_IMAGE` in the `boop-config` ConfigMap.

The default overlay pins `:stable`. For reproducible rollouts, pin a
specific tag (`:v0.1.0`) in the overlay.

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
│       ├── opencode.json
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

The home cluster. Pins `:latest` for both images (main HEAD); bump to
`:stable` or a specific tag for reproducible rollouts. Sets
`namespace: dev-tools` and adds the `cluster: pugquilt` label
(non-selector). Ingress: Traefik `IngressRoute` for
`Host(boop.qubitquilt.dev) && PathPrefix(/webhook)` → `boop-receiver:http`
on `websecure`.

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

Receiver:

```
cd apps/receiver
make docker-build docker-push IMAGE=ghcr.io/qubitquilt/boop-receiver:0.2.0
# edit apps/k8s/overlays/pugquilt/kustomization.yaml to pin newTag: 0.2.0
git commit -am "bump receiver to 0.2.0"
git push   # ArgoCD syncs
```

Runner:

```
cd apps/runner
make docker-build docker-push IMAGE=ghcr.io/qubitquilt/boop-runner:0.2.0
# edit apps/k8s/base/config.yaml JOB_IMAGE to point at the new tag
# (or override per-cluster)
git commit -am "bump runner to 0.2.0"
git push
```

The receiver is restarted by the Deployment rollout; the runner image
is consumed on the next Job submit. No coordination required.

## Image layout

| Image | Base | Size | Why |
|---|---|---|---|
| `boop-receiver` | `gcr.io/distroless/static-debian12:nonroot` | small (~15 MB) | Static, no shell, no package manager, non-root. |
| `boop-runner` | `ubuntu:24.04` + Node 22 + `opencode-ai` | larger (~500 MB) | The `opencode-ai` binary needs glibc 2.39; Ubuntu 24.04 ships it. Debian 12's glibc 2.36 deadlocks the binary. |

Both are built `linux/arm64` only. The cluster nodes are arm64; the
`boop-runner-set` Actions runners are arm64.

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
