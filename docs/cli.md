# boop CLI

A command-line client for the `boop-receiver` HTTP API.

- **AI agents** (primary): `--json` flag, env-driven config, no
  interactive prompts, predictable exit codes. Errors print as
  `{"error":{"status":N,"body":"..."}}` in JSON mode.
- **Humans**: human-readable tables by default, with `--json` for
  piping to `jq`.

The CLI lives in `apps/cli/` and is a separate Go module
(`github.com/michaelruelas/boop-cli`). It builds, tests, and
versions independently of the receiver.

## Connecting to a receiver

The CLI defaults to the in-cluster service address:

```
BOOP_API_URL=http://boop-receiver.dev-tools.svc.cluster.local:8080
```

Set this when running from inside the cluster (e.g. from another
pod) or from an AI agent colocated in the same K8s context.

### Local (port-forward)

```sh
kubectl -n dev-tools port-forward svc/boop-receiver 8080:8080 &
export BOOP_API_URL=http://localhost:8080
export BOOP_RUNNER_TOKEN=$(kubectl -n dev-tools get secret boop-secrets -o jsonpath='{.data.runner-token}' | base64 -d)
boop health
```

The read-only endpoints (`/api/reviews`, `GET /api/runs`,
`/api/stats`, `/api/installations`) are unauthenticated at the
receiver layer. They leak Job metadata only (no secrets, no PR
content). The POST routes under `/api/runs/{id}/*` (telemetry,
status, re-run) require `X-BOOP-Runner-Token`. Exposing the API
outside the cluster is tracked in QUB-115.

## Install

```sh
cd apps/cli
make build        # -> bin/boop
make install      # or copy to $GOPATH/bin/boop
```

Or via Docker / distroless:

```sh
make docker-build  # ghcr.io/qubitquilt/boop-cli:latest
```

## Configuration

Config resolves from three layers, lowest to highest precedence:

1. **Built-in default**: `api_url` =
   `http://boop-receiver.dev-tools.svc.cluster.local:8080` (the
   in-cluster service name).
2. **`$XDG_CONFIG_HOME/boop/config.json`** (falls back to
   `~/.config/boop/config.json`). Optional; materialized by
   `boop config write`. File mode is `0600` — it may hold the runner
   token.
3. **Environment variables**: `BOOP_API_URL`,
   `BOOP_RUNNER_TOKEN`, `BOOP_DASHBOARD_TOKEN`. These always win.
   Empty env vars are no-ops (they do not blank a populated file).

Inspect the resolved config:

```sh
boop config show
boop config path
```

Write a config file (e.g. for a local dev receiver behind a
`kubectl port-forward`):

```sh
boop config write \
  --api-url http://localhost:8080 \
  --runner-token "$(kubectl -n dev-tools get secret boop-secrets -o jsonpath='{.data.runner-token}' | base64 -d)"
```

`--api-url` is validated at write time. A string without a scheme
and host is rejected.

## Global flags

| Flag        | Purpose                              |
|-------------|--------------------------------------|
| `--json`    | Output raw JSON instead of tables    |
| `--timeout` | Request timeout (default 30s)        |
| `--version` | Print version and exit               |
| `--short`   | With `--version`: print SHA only     |

Flags work in any position. `boop runs --json list` and
`boop --json runs list` both produce JSON output. Same for
`--version`.

## Commands

### `boop health`

Check the receiver is up. The receiver returns plain text `"ok"` (not
JSON), so the CLI synthesizes a `{"status":"ok"}` body for `--json`.

```sh
boop health              # -> boop receiver: ok
boop health --json       # -> {"status":"ok"}
```

### `boop reviews`

Snapshot of review Jobs in the receiver namespace, bucketed into
`active` / `recent` / `failed`. This mirrors
`GET /api/reviews` exactly — it is a K8s API read, not the SQLite
data layer. Runs GCd after the Job TTL (1h) do not appear here even
if their store row survives.

```sh
boop reviews
boop reviews --json
```

### `boop installations`

List the GitHub App installations the receiver knows about (from the
SQLite `installations` table, refreshed by a background poller every 5
min). Columns include `paused` and `lens_opt_out` (QUB-108/QUB-111).

```sh
boop installations
boop installations --json
```

### `boop runs list`

Paginated, filterable run history. Query params match
`GET /api/runs`:

| Flag             | Purpose                                       |
|------------------|-----------------------------------------------|
| `--owner`        | Filter by owner (exact match)                 |
| `--repo`         | Filter by repo (exact match)                  |
| `--status`       | `pending|running|succeeded|failed`            |
| `--installation` | Filter by installation id                     |
| `--from` / `--to`| Inclusive RFC3339 bounds on `started_at`      |
| `--cursor`       | Paginated cursor from a previous `next_cursor`|
| `--limit`        | Page size (1..200, default 50)                |

```sh
boop runs list --status failed --from 2026-08-01T00:00:00Z --limit 20
boop runs list --owner qubitquilt --repo boop --json | jq '.runs[-1]'
```

Each row carries its telemetry inline (when recorded). Use `--json`
to drill into the `telemetry` block.

### `boop runs get <run-id>`

Show a single run by id (the K8s Job name, e.g.
`boop-qubitquilt-boop-42-a1b2c3d`) plus its telemetry. Uses the
dedicated `GET /api/runs/{id}` endpoint for a single-point lookup.

```sh
boop runs get boop-qubitquilt-boop-42-a1b2c3d
boop runs get boop-qubitquilt-boop-42-a1b2c3d --json
```

### `boop runs rerun <run-id>`

Re-run a terminal run (QUB-110). This calls
`POST /api/runs/{id}/rerun`, which mints a new K8s Job with a
`-rN` suffix and backfills `parent_run_id` / `superseded_by_id`
lineage. Requires the runner token and a `--reason` (free text,
logged to the audit trail). The POST is retried on transient
5xx errors because the receiver de-dupes on run ID.

Without `--yes`, the CLI fetches
`GET /api/runs/{id}/rerun-preview`, renders the diff (prior run vs.
the proposed new job), and prints the exact command to confirm:

```sh
boop runs rerun boop-qubitquilt-boop-42-a1b2c3d --reason "fixed the lint failure"
boop runs rerun boop-qubitquilt-boop-42-a1b2c3d --reason "retry after infra flap" --yes
```

Status code mapping:
- `404` -> "run not found" (the Job was GCd and the store row pruned)
- `401` -> unauthorized (runner token missing or wrong)
- `409` -> "not in a terminal state" (cannot re-run an in-flight review)
- `400` -> echoes the receiver body (e.g. "reason is required")

### `boop stats`

Dashboard aggregations over a time window (default: last 30 days).
Query params match `GET /api/stats`:

| Flag     | Purpose                         |
|----------|---------------------------------|
| `--from` | Inclusive RFC3339 lower bound   |
| `--to`   | Inclusive RFC3339 upper bound   |
| `--bucket`| `hour|day|week` (default day)  |

```sh
boop stats --from 2026-07-01T00:00:00Z
boop stats --bucket week --json
```

## Retries

GET requests are retried (up to 3 times) on transient 5xx or
connection errors with a short backoff. The rerun POST endpoint is
also retried because the receiver de-dupes on run ID, making the
request idempotent. Retry attempts are logged to stderr.

## Exit codes

| Code | Meaning                                      |
|------|----------------------------------------------|
| 0    | Success                                      |
| 1    | API error (non-2xx), auth failure, network error, or usage error |
| 2    | (reserved; not currently emitted)            |

All errors from subcommands and flag-parsing errors exit with code 1.
The stdlib `flag` package would exit 2 on parse errors, but this CLI
uses `flag.ContinueOnError` and re-wraps parse errors as usage errors
that exit 1.

## Agent usage notes

- `boop runs list --json | jq -r '.runs[] | select(.run.status=="failed") | .run.id'`
  -> feed into `boop runs get` or `boop runs rerun`.
- The CLI is pure Go with no runtime dependencies beyond the stdlib;
  the distroless Docker image is ~12MB and works in any container
  that can reach the receiver `/api/*` endpoints.
- For in-cluster use, set `BOOP_API_URL` to the
  `boop-receiver.dev-tools:8080` ClusterIP — no config file needed.

## Related

- [receiver.md](./receiver.md) — the receiver HTTP API + types
- [architecture.md](./architecture.md) — system flow
- [webhook-contract.md](./webhook-contract.md) — events, dedup, status thread