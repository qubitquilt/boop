# boop CLI

> Full documentation: [`docs/cli.md`](../../docs/cli.md)

A command-line client for the boop receiver API. Designed for two
audiences:

- **AI agents** (primary): `--json` flag, env-driven config, no
  interactive prompts, predictable exit codes.
- **Humans** doing ad-hoc investigation: human-readable tables by
  default, with `--json` as an escape hatch for piping to `jq`.

## Install

```sh
cd apps/cli
make build        # → bin/boop
make install      #   or copy to $GOPATH/bin/boop
```

Or via Docker:

```sh
make docker-build  # ghcr.io/qubitquilt/boop-cli:latest
```

## Configuration

Config resolves from three layers, lowest to highest precedence:

1. Built-in default: `api_url` = `http://boop-receiver.dev-tools.svc.cluster.local:8080`
   (the in-cluster service name).
2. `$XDG_CONFIG_HOME/boop/config.json` (falls back to
   `~/.config/boop/config.json`). Optional; materialized by
   `boop config write`.
3. Environment variables: `BOOP_API_URL`, `BOOP_RUNNER_TOKEN`,
   `BOOP_DASHBOARD_TOKEN`. These always win. Empty env vars are
   no-ops (they don't blank a populated file).

Inspect the resolved config:

```sh
boop config show
boop config path
```

Write a config file (e.g. for a local dev receiver behind a port-forward):

```sh
boop config write --api-url http://localhost:8080 --runner-token $(kubectl get secret boop-secrets -o jsonpath='{.data.runner-token}' | base64 -d)
```

## Usage

### `boop health`

Check the receiver is up.

```sh
boop health              # → boop receiver: ok
boop health --json       # → {"status":"ok"}
```

### `boop reviews`

Snapshot of review Jobs in the receiver's namespace, bucketed into
active / recent / failed (K8s view, same shape as `/api/reviews`).

```sh
boop reviews
```

### `boop installations`

List the GitHub App installations the receiver knows about.

```sh
boop installations
boop installations --json
```

### `boop runs list`

Paginated, filterable run history. Query params match `/api/runs`:

| Flag            | Purpose                                      |
|-----------------|----------------------------------------------|
| `--owner`       | Filter by owner (exact match)                |
| `--repo`        | Filter by repo (exact match)                 |
| `--status`      | `pending\|running\|succeeded\|failed`        |
| `--installation`| Filter by installation id                    |
| `--from` / `--to`| Inclusive RFC3339 bounds on `started_at`   |
| `--cursor`      | Paginated cursor from a previous `next_cursor`|
| `--limit`       | Page size (1..200, default 50)               |

```sh
boop runs list --status failed --from 2026-08-01T00:00:00Z --limit 20
boop runs list --owner qubitquilt --repo boop --json | jq '.runs[-1]'
```

### `boop runs get <run-id>`

Show a single run (id = the K8s Job name, e.g.
`boop-qubitquilt-boop-42-a1b2c3d`) plus its telemetry. The receiver
has no `GET /api/runs/{id}` endpoint, so this lists client-side
and filters. Use `--json` for the raw `RunWithTelemetry` shape.

```sh
boop runs get boop-qubitquilt-boop-42-a1b2c3d
```

### `boop runs rerun <run-id>`

Re-run a terminal run (QUB-110). Without `--yes`, prints a
`RerunPreviewResponse` diff and the exact command to confirm with.
Requires `--reason` (free text, logged to the audit trail) and the
runner token.

```sh
boop runs rerun boop-qubitquilt-boop-42-a1b2c3d --reason "fixed the lint failure"
boop runs rerun boop-qubitquilt-boop-42-a1b2c3d --reason "retry after infra flap" --yes
```

### `boop stats`

Dashboard aggregations over a time window (default: last 30 days).
Query params match `/api/stats`:

| Flag     | Purpose                        |
|----------|--------------------------------|
| `--from` | Inclusive RFC3339 lower bound  |
| `--to`   | Inclusive RFC3339 upper bound  |
| `--bucket`| `hour\|day\|week` (default day)|

```sh
boop stats --from 2026-07-01T00:00:00Z
boop stats --bucket week --json
```

## Global flags

| Flag       | Purpose                              |
|------------|--------------------------------------|
| `--json`   | Output raw JSON (machine-readable)   |
| `--timeout`| Request timeout (default 30s)        |
| `--version`| Print version and exit               |

## Exit codes

- `0` — success
- `1` — API error (non-2xx from the receiver), auth failure, network
  error, or usage error (unknown command, missing required flag,
  malformed input). All error paths in the CLI exit with 1.

## Retries

GET requests are retried (up to 3 times) on transient 5xx / connection
errors with a short backoff. POST endpoints (`runs rerun`) are not
retried — the receiver's re-run flow is operator-initiated and
idempotency is the operator's responsibility.

## Related

- [docs/receiver.md](../receiver.md) — the receiver's HTTP API + types
- [docs/architecture.md](../architecture.md) — system flow
