# boop CLI — AI Agent Guide

The boop CLI is a Go module in `apps/cli/`. It is a single-binary,
stdlib-only HTTP client for the boop-receiver API. It has no framework
dependencies — no cobra, no viper, no CLI library. The design is
intentionally minimal so the CLI can be built, tested, and versioned
independently of the receiver.

## Entry point

`cmd/boop/main.go` exports `runMain(c *cli, raw []string) error` which
tests call directly. `main()` wraps it with `os.Args` + `os.Exit`.

The `cli` struct carries `stdout`, `stderr`, `args`, `asJSON`, and
`timeout`. All subcommands are methods on this struct, dispatched by a
flat switch in `run()`.

## Adding a subcommand

1. Add a method on `cli` (e.g. `cmdFoo(ctx, args) error`).
2. Add a case to the switch in `run()`.
3. If the command calls the receiver API, add the endpoint method to
   `internal/client/client.go`.
4. If the command returns a new type, add the type to
   `internal/api/types.go`.
5. Add a human table renderer in `internal/render/render.go` (the
   `renderHuman` switch).
6. Add CLI integration tests in `cmd/boop/main_test.go`.

## Key conventions

- **No framework.** Flags use `flag.NewFlagSet` with `ContinueOnError`
  and `io.Discard` output. Errors from flag parsing are re-wrapped as
  `usageError`.
- **`--json` and `--version` are position-independent.** The pre-scan
  in `runMain()` extracts them from `os.Args[1:]` before `flag.Parse`.
  New global flags that need the same treatment must be added to the
  pre-scan loop.
- **JSON error envelope.** When `--json` is set, `printErr` wraps
  errors in `{"error":{"status":N,"body":"..."}}`. The `status` field
  is populated from `*client.ErrAPI` when available.
- **Idempotent POST retry.** Client methods that POST to idempotent
  endpoints register their path prefix with `WithIdempotentPOST` at
  the call site (in `client()`).
- **Tests use `runMain` directly.** They pass `*bytes.Buffer` for
  stdout/stderr and never call `main()`.

## Package structure

```
cmd/boop/main.go       — entrypoint, dispatch, usage/error printing
internal/api/types.go  — JSON shapes mirroring the receiver API
internal/client/       — HTTP client, retry logic, typed errors
internal/config/       — layered config (defaults, file, env)
internal/render/       — human tables + JSON serialization
```

## Global flags

| Flag        | Scope      | Pre-scanned? |
|-------------|------------|-------------|
| `--json`    | Any position | yes        |
| `--version` | Any position | yes        |
| `--timeout` | Global only  | no         |
| `--short`   | With `--version` | no (FlagSet handles it) |
| `--help`    | Global + subcommand | no |

## Tests

- `cmd/boop/main_test.go` — integration tests for flag parsing,
  dispatch, error envelopes, config validation, and runs get.
- `internal/client/client_test.go` — HTTP client tests with
  `httptest.Server`, including retry logic.
- `internal/render/render_test.go` — table and JSON output shape
  tests.
- `internal/config/config_test.go` — config load/write round-trips.

Run: `go test ./...` in `apps/cli/`.

## Build

```sh
make build   # -> bin/boop
make test    # go test ./...
```

The Docker image is distroless/static, ~12MB. No runtime
dependencies.