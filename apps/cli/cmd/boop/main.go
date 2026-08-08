// Command boop is the CLI for inspecting and managing the boop
// receiver. It is intended for two audiences:
//
//   - AI agents (the primary use case): machine-parseable output via
//     `--json`, env-var configuration, no interactive prompts.
//   - Humans doing ad-hoc investigation: human-readable tables by
//     default, with `--json` as an escape hatch for piping to jq.
//
// Usage:
//
//	boop reviews                       list active/recent/failed reviews
//	boop health                        check the receiver is up
//	boop installations                 list GitHub App installations
//	boop runs list [flags]             list runs (filterable)
//	boop runs get <run-id>             show a single run + telemetry
//	boop runs rerun <run-id> --reason  re-run a terminal run
//	boop stats [flags]                 dashboard aggregations
//	boop config show                   show resolved config
//	boop config path                   show the config file path
//	boop config write --runner-token   write config.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/michaelruelas/boop-cli/internal/client"
	"github.com/michaelruelas/boop-cli/internal/config"
	"github.com/michaelruelas/boop-cli/internal/render"
)

// version is set at build time via -ldflags.
var version = "dev"

// shortVersion extracts the commit SHA from a version string like
// "v0.1.0-141-gdbeb110-dirty" and returns just "gdbeb110". Falls
// back to the whole string when no SHA pattern is found.
func shortVersion(v string) string {
	// Pattern: "v0.1.0-141-gdbeb110-dirty" → "gdbeb110"
	if i := strings.LastIndex(v, "-g"); i >= 0 {
		s := v[i+1:]
		if j := strings.Index(s, "-"); j >= 0 {
			s = s[:j]
		}
		if s != "" {
			return s
		}
	}
	return v
}

// cli is the top-level command. All subcommands register as methods
// on it so they share the --json flag, the http client, and the
// context timeout.
type cli struct {
	stdout, stderr io.Writer
	args           []string
	asJSON         bool
	timeout        time.Duration
}

func main() {
	c := &cli{stdout: os.Stdout, stderr: os.Stderr}
	if err := runMain(c, os.Args[1:]); err != nil {
		// runMain already printed the error to c.stderr.
		os.Exit(1)
	}
}

// runMain is the testable entry point. It parses flags, dispatches the
// subcommand, and returns an error on failure. The caller supplies the
// raw args (os.Args[1:]) so tests can inject synthetic values without
// touching the global os.Args.
func runMain(c *cli, raw []string) error {
	// Pre-scan for --json and --version so they work in any
	// position (e.g. `boop runs --json list`). Flag.Parse stops
	// at the first non-flag arg, so --json after a subcommand
	// would be invisible to the global flag set. We use a
	// separate var for the pre-scan to avoid flag registration
	// overwriting c.asJSON with the default value (false).
	filtered := make([]string, 0, len(raw))
	jsonSeen := false
	versionSeen := false
	for _, a := range raw {
		switch a {
		case "--json":
			jsonSeen = true
		case "--version":
			versionSeen = true
		default:
			filtered = append(filtered, a)
		}
	}

	fs := flag.NewFlagSet("boop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "request timeout")
	shortFlag := fs.Bool("short", false, "print short version (SHA only)")
	helpFlag := fs.Bool("help", false, "print usage and exit")
	fs.Parse(filtered)

	// Apply pre-scan results after Parse so they don't get
	// clobbered by flag registration defaults.
	c.asJSON = jsonSeen
	if versionSeen {
		if *shortFlag {
			fmt.Fprintln(c.stdout, shortVersion(version))
		} else {
			fmt.Fprintln(c.stdout, version)
		}
		return nil
	}
	if *helpFlag || isHelpArg(filtered) {
		printUsage(c.stdout)
		return nil
	}
	c.args = fs.Args()
	ctx := context.Background()
	if err := c.run(ctx); err != nil {
		printErr(c.stderr, err, c.asJSON)
		return err
	}
	return nil
}

// isHelpArg reports whether the (already-stripped-of-flags) args
// begin with a help-style token. This is the fallback when flag.Parse
// hasn't consumed -h (e.g. when the user wrote `boop help review`).
func isHelpArg(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

// run dispatches to the subcommand. Each subcommand is a method so
// the --json/timeout flags are already parsed. Unknown subcommands
// print usage.
func (c *cli) run(ctx context.Context) error {
	if len(c.args) == 0 {
		return errUsage("boop: no command given (try --help)")
	}
	cmd := c.args[0]
	rest := c.args[1:]
	switch cmd {
	case "reviews":
		return c.cmdReviews(ctx, rest)
	case "health":
		return c.cmdHealth(ctx, rest)
	case "installations":
		return c.cmdInstallations(ctx, rest)
	case "runs":
		return c.cmdRuns(ctx, rest)
	case "stats":
		return c.cmdStats(ctx, rest)
	case "config":
		return c.cmdConfig(ctx, rest)
	case "-h", "--help", "help":
		printUsage(c.stdout)
		return nil
	default:
		return errUsage(fmt.Sprintf("boop: unknown command %q (try --help)", cmd))
	}
}

// clientFor builds an api.Client from the resolved config. The
// receiver token is optional — only POST endpoints require it, and
// those check at request time.
func (c *cli) client() (*client.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	cli := client.New(cfg.APIURL, cfg.RunnerToken)
	// Mark the rerun POST endpoint as idempotent so transient 5xx
	// errors are retried automatically.
	cli = cli.WithIdempotentPOST("/api/runs/")
	return cli, nil
}

// --- health ---

func (c *cli) cmdHealth(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errUsage("boop health: takes no arguments")
	}
	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	h, err := cli.Health(ctx)
	if err != nil {
		return err
	}
	return render.Render(c.stdout, h, c.asJSON)
}

// --- reviews ---

func (c *cli) cmdReviews(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errUsage("boop reviews: takes no arguments")
	}
	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r, err := cli.ListReviews(ctx)
	if err != nil {
		return err
	}
	return render.Render(c.stdout, r, c.asJSON)
}

// --- installations ---

func (c *cli) cmdInstallations(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errUsage("boop installations: takes no arguments")
	}
	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r, err := cli.ListInstallations(ctx)
	if err != nil {
		return err
	}
	return render.Render(c.stdout, r, c.asJSON)
}

// --- runs ---

func (c *cli) cmdRuns(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage("boop runs: expected `list`, `get`, or `rerun` (see `boop runs --help`)")
	}
	switch args[0] {
	case "list":
		return c.cmdRunsList(ctx, args[1:])
	case "get":
		return c.cmdRunsGet(ctx, args[1:])
	case "rerun":
		return c.cmdRunsRerun(ctx, args[1:])
	case "-h", "--help", "help":
		printRunsUsage(c.stdout)
		return nil
	default:
		return errUsage(fmt.Sprintf("boop runs: unknown subcommand %q (expected list, get, rerun)", args[0]))
	}
}

func (c *cli) cmdRunsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runs list", flag.ContinueOnError)
	// We capture errors into a discard to avoid flag.FlagSet
	// printing its own usage text on -h; the errUsage handler
	// below prints our usage instead.
	fs.SetOutput(io.Discard)
	o := client.ListRunsOpts{}
	fs.StringVar(&o.Owner, "owner", "", "filter by owner (exact match)")
	fs.StringVar(&o.Repo, "repo", "", "filter by repo (exact match)")
	fs.StringVar(&o.Status, "status", "", "filter by status (pending/running/succeeded/failed)")
	var install int64
	fs.Int64Var(&install, "installation", 0, "filter by installation id")
	fs.StringVar(&o.Cursor, "cursor", "", "paginated cursor from a previous --next")
	var limit int
	fs.IntVar(&limit, "limit", 0, "page size (1..200, default 50)")
	var from, to string
	fs.StringVar(&from, "from", "", "inclusive lower bound (RFC3339)")
	fs.StringVar(&to, "to", "", "inclusive upper bound (RFC3339)")
	if err := fs.Parse(args); err != nil {
		return errUsage("boop runs list: " + err.Error())
	}
	if err := parseTime("from", from, &o.From); err != nil {
		return err
	}
	if err := parseTime("to", to, &o.To); err != nil {
		return err
	}
	o.Limit = limit
	o.Install = install

	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r, err := cli.ListRuns(ctx, o)
	if err != nil {
		return err
	}
	return render.Render(c.stdout, r, c.asJSON)
}

// parseTime parses an RFC3339 string into the target, skipping when
// the string is empty. Returns a user-facing error on parse failure.
func parseTime(label, s string, dst *time.Time) error {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("boop: --%s: parse %q: expected RFC3339 (e.g. 2006-01-02T15:04:05Z): %w", label, s, err)
	}
	*dst = t
	return nil
}

func (c *cli) cmdRunsGet(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errUsage("boop runs get: expected exactly one run-id argument")
	}
	runID := args[0]
	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	r, err := cli.GetRun(ctx, runID)
	if err != nil {
		if apiErr, ok := err.(*client.ErrAPI); ok && apiErr.StatusCode == 404 {
			if c.asJSON {
				return err
			}
			return fmt.Errorf("boop: run %q not found (check the id; runs are GC'd 1h after completion)", runID)
		}
		return err
	}
	return render.Render(c.stdout, r, c.asJSON)
}

func (c *cli) cmdRunsRerun(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage("boop runs rerun: expected <run-id> (see --help)")
	}
	runID := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet("runs rerun", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var reason string
	var yes bool
	fs.StringVar(&reason, "reason", "", "required: why you're re-running (free text, for the audit log)")
	fs.BoolVar(&yes, "yes", false, "skip the preview and confirm directly")
	if err := fs.Parse(rest); err != nil {
		return errUsage("boop runs rerun: " + err.Error())
	}
	if reason == "" {
		return errUsage("boop runs rerun: --reason is required")
	}

	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if !yes {
		// Show the preview, then require explicit confirmation.
		preview, err := cli.RerunPreview(ctx, runID)
		if err != nil {
			if apiErr, ok := err.(*client.ErrAPI); ok && apiErr.StatusCode == 404 {
				return fmt.Errorf("boop: run %q not found", runID)
			}
			return err
		}
		if err := render.Render(c.stdout, preview, c.asJSON); err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, "\nto confirm, re-run with --yes and a --reason:")
		fmt.Fprintf(c.stdout, "  boop runs rerun %s --reason %q --yes\n", runID, reason)
		return nil
	}

	resp, err := cli.Rerun(ctx, runID, reason)
	if err != nil {
		if apiErr, ok := err.(*client.ErrAPI); ok {
			switch apiErr.StatusCode {
			case 404:
				return fmt.Errorf("boop: run %q not found", runID)
			case 401:
				return fmt.Errorf("boop: unauthorized — check BOOP_RUNNER_TOKEN")
			case 409:
				return fmt.Errorf("boop: run %q is not in a terminal state (cannot re-run an in-flight review)", runID)
			case 400:
				return fmt.Errorf("boop: %s", apiErr.Body)
			}
		}
		return err
	}
	return render.Render(c.stdout, resp, c.asJSON)
}

// --- stats ---

func (c *cli) cmdStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := client.StatsOpts{}
	var from, to, bucket string
	fs.StringVar(&from, "from", "", "inclusive lower bound (RFC3339, default 30d ago)")
	fs.StringVar(&to, "to", "", "inclusive upper bound (RFC3339, default now)")
	fs.StringVar(&bucket, "bucket", "day", "time bucket: hour|day|week")
	if err := fs.Parse(args); err != nil {
		return errUsage("boop stats: " + err.Error())
	}
	if err := parseTime("from", from, &o.From); err != nil {
		return err
	}
	if err := parseTime("to", to, &o.To); err != nil {
		return err
	}
	o.Bucket = bucket

	cli, err := c.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r, err := cli.Stats(ctx, o)
	if err != nil {
		return err
	}
	return render.Render(c.stdout, r, c.asJSON)
}

// --- config ---

func (c *cli) cmdConfig(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage("boop config: expected `show`, `path`, or `write` (see --help)")
	}
	switch args[0] {
	case "show":
		return c.cmdConfigShow(ctx, args[1:])
	case "path":
		return c.cmdConfigPath(ctx, args[1:])
	case "write":
		return c.cmdConfigWrite(ctx, args[1:])
	case "-h", "--help", "help":
		printConfigUsage(c.stdout)
		return nil
	default:
		return errUsage(fmt.Sprintf("boop config: unknown subcommand %q (expected show, path, write)", args[0]))
	}
}

func (c *cli) cmdConfigShow(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errUsage("boop config show: takes no arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, _ := config.Path()
	fmt.Fprintf(c.stdout, "config file: %s\n", path)
	fmt.Fprintf(c.stdout, "api_url:      %s\n", cfg.APIURL)
	fmt.Fprintf(c.stdout, "runner token: %s\n", mask(cfg.RunnerToken))
	fmt.Fprintf(c.stdout, "dash token:   %s\n", mask(cfg.DashboardToken))
	return nil
}

func (c *cli) cmdConfigPath(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errUsage("boop config path: takes no arguments")
	}
	path, err := config.Path()
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, path)
	return nil
}

func (c *cli) cmdConfigWrite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config write", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		apiURL      string
		runnerToken string
		dashToken   string
	)
	fs.StringVar(&apiURL, "api-url", "", "receiver base URL (default: built-in)")
	fs.StringVar(&runnerToken, "runner-token", "", "shared secret for POST endpoints")
	fs.StringVar(&dashToken, "dashboard-token", "", "BOOP_DASHBOARD_TOKEN equivalent")
	if err := fs.Parse(args); err != nil {
		return errUsage("boop config write: " + err.Error())
	}
	if apiURL != "" {
		u, err := url.Parse(apiURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return errUsage(fmt.Sprintf("boop config write: --api-url: invalid URL %q", apiURL))
		}
	}
	cfg := config.FileConfig{
		APIURL:         apiURL,
		RunnerToken:    runnerToken,
		DashboardToken: dashToken,
	}
	if err := config.Write(cfg); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "wrote %s\n", mustConfigPath())
	return nil
}

func mustConfigPath() string {
	p, _ := config.Path()
	return p
}

func mask(s string) string {
	if s == "" {
		return "(empty)"
	}
	return strings.Repeat("•", len(s))
}

// --- usage / errors ---

type usageError struct{ msg string }

func errUsage(msg string) error { return &usageError{msg: msg} }

func (e *usageError) Error() string { return e.msg }

// printErr renders an error to stderr. Flag/usage errors print the
// short usage hint; API errors print the receiver's body verbatim.
// When asJSON is true the error is wrapped in a JSON envelope so
// agents can parse errors structurally instead of grepping text.
func printErr(w io.Writer, err error, asJSON bool) {
	if asJSON {
		body := err.Error()
		status := 0
		if ae, ok := err.(*client.ErrAPI); ok {
			status = ae.StatusCode
		}
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"status": status,
				"body":   body,
			},
		})
		fmt.Fprintln(w, string(b))
		return
	}
	if ue, ok := err.(*usageError); ok {
		fmt.Fprintln(w, ue.msg)
		fmt.Fprintln(w, "run `boop --help` for usage")
		return
	}
	fmt.Fprintln(w, err)
}

func printUsage(w io.Writer) {
	io.WriteString(w, `boop — BoopPr receiver CLI

Usage:
  boop <command> [flags]

Commands:
  reviews         List active/recent/failed review Jobs (K8s snapshot)
  health          Check the receiver is up
  installations   List GitHub App installations
  runs list       List runs (filterable; --owner --repo --status --from --to --limit --cursor)
  runs get        Show a single run by id (+ telemetry)
  runs rerun      Re-run a terminal run (--reason required --yes to skip preview)
  stats           Dashboard aggregations (--from --to --bucket)
  config show     Show resolved config
  config path     Print the config file path
  config write    Write config.json (--api-url --runner-token --dashboard-token)

Flags (global):
  --json          Output raw JSON instead of human tables
  --timeout       Request timeout (default 30s)
  --version       Print version and exit
`)
}

func printRunsUsage(w io.Writer) {
	io.WriteString(w, `boop runs — subcommand required

Usage:
  boop runs list [flags]
  boop runs get <run-id>
  boop runs rerun <run-id> --reason <text> [--yes]

Flags for "runs list":
  --owner       Filter by owner (exact match)
  --repo        Filter by repo (exact match)
  --status      Filter by status (pending|running|succeeded|failed)
  --installation  Filter by installation id
  --from        Inclusive lower bound (RFC3339)
  --to          Inclusive upper bound (RFC3339)
  --cursor      Paginated cursor from a previous --next
  --limit       Page size (1..200, default 50)

Flags for "runs rerun":
  --reason   Required. Why you're re-running (audit log).
  --yes      Skip the preview and confirm directly.
`)
}

func printConfigUsage(w io.Writer) {
	io.WriteString(w, `boop config — subcommands: show, path, write

Usage:
  boop config show
  boop config path
  boop config write [--api-url URL] [--runner-token TOKEN] [--dashboard-token TOKEN]
`)
}
