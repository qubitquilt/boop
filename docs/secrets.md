# Secrets

Credentials Boop needs and where they live. See also
[deployment.md](./deployment.md), [receiver.md](./receiver.md#configuration).

## Inventory

| Secret | Format | Where stored | Where consumed |
|---|---|---|---|
| `webhook-secret` | opaque string | OpenBao `secret/dev-tools/boop` | Receiver (HMAC verify) + GitHub App config |
| `app-id` | integer | OpenBao `secret/dev-tools/boop` | Receiver + runner (env `GITHUB_APP_ID`) |
| `installation-id` | integer | OpenBao `secret/dev-tools/boop` | Receiver + runner (env `GITHUB_APP_INSTALLATION_ID`) |
| `private-key` | PEM (PKCS#1 RSA) | OpenBao `secret/dev-tools/boop` | Receiver + runner (env `GITHUB_APP_PRIVATE_KEY`) |
| `openrouter-api-key` | opaque string | OpenBao `secret/dev-tools/boop` | Runner (env `OPENROUTER_API_KEY`) |
| `runner-token` | opaque string (32-byte hex recommended) | OpenBao `secret/dev-tools/boop` | Receiver + runner (env `RUNNER_TOKEN` / `BOOP_DASHBOARD_TOKEN`); auth on the runner's POST endpoints |

The GitHub App login (`BoopPr[bot]`) is **not** a secret; it is
hard-coded in the receiver Deployment as `BOT_LOGIN`.

The `runner-token` is a shared secret between the receiver and the
runner. The receiver authenticates every runner POST (telemetry +
status) against it; the runner carries it as `BOOP_DASHBOARD_TOKEN`
in the Job template. Generate once with `openssl rand -hex 32` and
store alongside the other credentials.

## OpenBao wiring

The credentials live in a single OpenBao path: `secret/dev-tools/boop`.
The keys above are the OpenBao keys (not the K8s Secret keys). The K8s
Secret `boop-secrets` is populated from OpenBao via a `ClusterSecretStore`
(an External Secrets Operator pattern) and then referenced by the
Deployment and Job template.

```
OpenBao path: secret/dev-tools/boop
  ├─ webhook-secret         → K8s Secret key WEBHOOK_SECRET
  ├─ app-id                 → K8s Secret key GITHUB_APP_ID
  ├─ installation-id        → K8s Secret key GITHUB_APP_INSTALLATION_ID
  ├─ private-key            → K8s Secret key GITHUB_APP_PRIVATE_KEY
  ├─ openrouter-api-key     → K8s Secret key OPENROUTER_API_KEY
  └─ runner-token           → K8s Secret key RUNNER_TOKEN
```

### Updating a secret

```
# Via the OpenBao CLI
bao kv put secret/dev-tools/boop \
    webhook-secret="$(openssl rand -hex 32)" \
    app-id=... \
    installation-id=... \
    private-key=@/path/to/key.pem \
    openrouter-api-key=... \
    runner-token="$(openssl rand -hex 32)"

# External Secrets Operator resyncs the K8s Secret on its poll interval
# (or via a manual annotation: kubectl annotate externalsecret boop-secrets -n dev-tools force-sync=$(date +%s))
```

A receiver restart is not required for credential rotation; the receiver
reads the env at startup. The next pod (after a normal rollout) picks up
the new values.

## GitHub App setup walkthrough

If you are creating a new App (or rotating the existing one's key):

1. **Create the App** in the org settings → Developer settings → GitHub
   Apps → New GitHub App.
2. **Name:** `BoopPr`. **Homepage URL:** whatever (it is not linked
   from the bot's PR footer). **Webhook URL:**
   `https://boop.qubitquilt.dev/webhook`. **Webhook secret:** a fresh
   32-byte hex string. Save this — it becomes `webhook-secret` in
   OpenBao.
3. **Permissions:** as listed in [deployment.md](./deployment.md#github-app-setup).
4. **Subscribe to events:** `Pull request`, `Issue comment`.
5. **Create.** Capture the App ID — it becomes `app-id`.
6. **Generate a private key.** Download the `.pem`. That is the
   `private-key` value in OpenBao.
7. **Install the App** on the `qubitquilt` org. Capture the
   installation ID at the end of the install URL
   (`…/installations/<id>`). That is the `installation-id`.

After installation, the App is ready to receive webhooks. Test by
opening a PR in any `qubitquilt` repo.

## Local dev: no OpenBao required

When running the receiver locally (`make run` in `apps/receiver`), the
credentials come from the local shell, not OpenBao:

```
export WEBHOOK_SECRET=$(openssl rand -hex 32)
export GITHUB_APP_ID=...
export GITHUB_APP_INSTALLATION_ID=...
export GITHUB_APP_PRIVATE_KEY="$(cat /path/to/key.pem)"
export KUBECONFIG=~/.kube/config
```

The receiver does not care where the values came from; only that they
are set and that the `GITHUB_APP_PRIVATE_KEY` parses as PKCS#1 RSA.

For end-to-end webhook testing, use
[smee.io](https://smee.io/) to forward a public URL to your local
receiver and point the GitHub App's webhook at the smee URL.

## Token lifetimes

Boop mints two kinds of tokens, both short-lived:

| Token | TTL | Where minted | Where used |
|---|---|---|---|
| GitHub App JWT | 10 min | Receiver (Go, `appJWT`), Runner (Node, `jwt.sign`) | App API (mint installation token) |
| GitHub installation token | 1 h | GitHub `/app/installations/{id}/access_tokens` | All Octokit calls in receiver + runner |

The receiver caches the installation token in-memory until (expiry − 5
min). The runner re-mints per pod (one token per Job). No cross-pod
sharing; no persistent storage.

The OpenRouter API key is a long-lived secret in OpenBao; it is read by
the runner from `boop-secrets` and passed straight through to the model
provider. OpenRouter does its own rate limiting and per-key accounting.

## Rotation

| Secret | Rotation cadence | Procedure |
|---|---|---|
| `webhook-secret` | on suspicion of leak | OpenBao update + GitHub App webhook config update. Re-deliveries during the gap will fail HMAC verify and return 401. |
| `app-id`, `installation-id` | on App migration | Generate new App; treat as a new install. There is no rotation in place — these are stable per App. |
| `private-key` | on suspicion of leak (yearly recommended) | Generate new key in App settings; revoke old in App settings; update OpenBao. Old key stops working immediately on revoke. |
| `openrouter-api-key` | on suspicion of leak; OpenBao key per environment | Generate in OpenRouter dashboard; update OpenBao. |

## Audit

Receiver logs every job creation with `pr`, `sha`, `reason`,
`status_comment_id`, `reaction_comment_id`, `review_number` at INFO
level. There is no separate audit log of token mints. To audit token
mint history, query the GitHub App's installation events in the org
audit log (or the OpenRouter dashboard for LLM calls).

## See also

- [deployment.md](./deployment.md) — how the secrets are wired into
  the cluster.
- [receiver.md](./receiver.md) — what reads them.
- [runner.md](./runner.md#environment) — what reads them in the Job.
