// rtk adapter.
//
// QUB-85: the runner's file reads (today: the SKILL.md and the seven
// lens files from the ConfigMap mount; tomorrow: any file an agent
// reads inside the cloned PR) go through this adapter instead of
// hitting `fs.readFile` directly. The adapter shells out to
// `rtk read`, which compresses the output for LLM consumption and
// returns 60-90% fewer tokens for typical source code.
//
// The adapter falls back to raw `fs.readFile` when:
//   - the rtk binary is not on PATH (image built without the QUB-85
//     changes, local dev without rtk installed),
//   - BOOP_RTK_DISABLED=1 is set (operator kill switch),
//   - the rtk binary is on PATH but a read call throws (e.g. the
//     installed rtk is incompatible with the path or the
//     ConfigMap mount). The first failure flips the adapter to
//     raw mode for the rest of the run so the fallback log
//     fires once, not per file.
//
// The two failure modes look identical to callers: `readFile(path,
// "utf8")` returns a string. The adapter logs the fallback on the
// first call so an operator can tell why their reads are not
// compressed.
//
// The adapter does NOT use rtk's hook for shell rewriting. The
// issue's open question #1 is resolved as "explicit calls only" —
// the hook would silently rewrite every `execFile` call and the
// LLM would lose auditability of what actually ran. The adapter is
// the one place rtk is invoked.

import type { ExecFileLike, FsLike, RtkAdapter } from "../types.ts";

// Default binary name. Resolved once on the first call so a
// "command not found" surfaces as a clean ENOENT-shape error, not
// a multi-minute binary-search through PATH.
const DEFAULT_RTK_BIN = "rtk";

// Default per-line truncation. rtk's read accepts no per-line cap
// at the CLI level; we keep the value generous so a single deeply
// nested TypeScript object literal or a minified line cannot
// dominate the prompt. 4000 chars covers any realistic source line
// the prompt would benefit from seeing; beyond that, rtk's
// own formatting collapses the line into a continuation marker.
// The TOML filter in apps/runner/rtk/filters.toml re-applies the
// same cap as a defense in depth (a future caller that forgets
// to pass `--truncate-lines-at` still gets bounded output).
const DEFAULT_TRUNCATE_LINES_AT = 4000;

type RtkState = {
  source: "uninitialized" | "rtk" | "raw";
  binary: string | null;
  reason: string | null;
  logged?: boolean;
};

// whichRtk resolves the rtk binary path on PATH. Returns the
// absolute path or null. The check runs once per process via the
// `createRtkAdapter` memoization; subsequent reads reuse the
// captured result.
async function whichRtk(bin: string, execFile: ExecFileLike): Promise<string | null> {
  try {
    const { stdout } = await execFile("which", [bin], {
      timeout: 5_000,
    });
    const resolved = stdout.trim();
    return resolved.length > 0 ? resolved : null;
  } catch {
    return null;
  }
}

// smokeTestRtk runs the smallest possible rtk read against
// /dev/null (always present on every unix, always empty) so the
// adapter can fail fast on a broken local install (the macOS
// homebrew rtk 0.42.3 bug) without waiting until the first real
// skill file. The smoke test mirrors the production CLI shape
// exactly (read + --truncate-lines-at + path) so a failure mode
// in the binary's flag parser or its subshell handoff is caught
// here. The timeout is short — a working rtk returns in <100ms —
// so a stuck binary fails the smoke test rather than blocks
// init(). The test is best-effort: a rtk that happens to be slow
// is not a fault; only a rtk that errors is a fault.
async function smokeTestRtk(
  binary: string,
  execFile: ExecFileLike,
): Promise<boolean> {
  try {
    const { stdout, stderr } = await execFile(binary, [
      "read",
      "--truncate-lines-at",
      "1000",
      "--",
      "/dev/null",
    ], {
      env: { RTK_TELEMETRY_DISABLED: "1" } as NodeJS.ProcessEnv,
      timeout: 5_000,
    });
    // A working rtk on `/dev/null` returns an empty string.
    // A broken rtk that internally shells out to bash's `read`
    // builtin emits a stderr like `read: --: invalid option`
    // and exits non-zero. Treat any non-empty stderr as a
    // smoke-test failure even if the exit code is zero (a
    // future rtk that emits a deprecation warning to stderr
    // for `--` would still be usable). The `?? ""` guards the
    // optional stderr/stdout under noUncheckedIndexedAccess
    // (the test fixtures set both, the production rtk sets
    // neither for an empty file).
    return (stderr ?? "").length === 0 && (stdout ?? "").length === 0;
  } catch {
    return false;
  }
}

// readRaw is the fallback. Same signature as fs.readFile, no rtk
// involvement. Used when rtk is disabled or the binary is missing.
async function readRaw(fs: FsLike, path: string): Promise<string> {
  const data = await fs.readFile(path, "utf8");
  return typeof data === "string" ? data : String(data);
}

// readViaRtk shells out to `rtk read <path> --max-lines N
// --truncate-lines-at N` and returns the stdout. The adapter never
// reads stderr: rtk's stderr is diagnostic (tee file paths, the
// trust notice) and the LLM does not need it. If rtk exits
// non-zero, the caller gets the underlying execFile error.
async function readViaRtk(
  execFile: ExecFileLike,
  env: NodeJS.ProcessEnv,
  binary: string,
  path: string,
  options: { truncateLinesAt?: number } = {},
): Promise<string> {
  // The line cap was previously set here as `--max-lines N`. It
  // turned out to be too aggressive: SKILL.md is 654 lines and
  // review-test-quality is 268, so the 400-line cap silently
  // truncated the orchestrator's instructions on every review.
  // The cap is now owned by the rtk filter in
  // apps/runner/rtk/filters.toml (still a 10000-line safety net
  // against truly enormous files), and this CLI call passes
  // through whatever rtk defaults to. The per-line
  // `--truncate-lines-at` cap stays here as a tighter, more
  // meaningful bound: a single 4000-char line is the longest
  // realistic source line (deeply nested TS object literal,
  // long SQL string, minified bundle header); past that, rtk
  // collapses the line into a continuation marker which the
  // model would not benefit from seeing.
  const truncateLinesAt = options.truncateLinesAt ?? DEFAULT_TRUNCATE_LINES_AT;
  const args = [
    "read",
    "--truncate-lines-at",
    String(truncateLinesAt),
    "--",
    path,
  ];
  const { stdout } = await execFile(binary, args, {
    env: {
      ...env,
      // rtk's own per-call escape hatch. Set here so a single
      // execFile's output is filtered even if a higher-level
      // caller forgot to set the runner-side BOOP_RTK_DISABLED.
      // (Belt + suspenders: the runner-side flag is checked
      // before the adapter is invoked at all.)
      RTK_TELEMETRY_DISABLED: "1",
    } as NodeJS.ProcessEnv,
    timeout: 30_000,
    maxBuffer: 16 * 1024 * 1024,
  });
  return stdout;
}

// createRtkAdapter returns an object with `{ readFile, enabled,
// binary, source }` for the runner's deps. The returned readFile
// is the only call site the rest of the runner uses; everything
// else is bookkeeping.
//
// `source` records how the adapter is configured:
//   - "rtk"   — rtk is on PATH and enabled, reads go through it
//   - "raw"   — reads go straight to fs (rtk disabled, missing,
//               or fall-through after an rtk error)
//
// The source is logged on first use so an operator tailing the
// pod logs can see whether rtk is in the read path. Subsequent
// calls do not re-log (the value is stable for the life of the
// pod).
export function createRtkAdapter({
  execFile,
  env = process.env,
  fs,
  log = () => {},
  disabled = false,
  binary = DEFAULT_RTK_BIN,
}: {
  execFile: ExecFileLike;
  env?: NodeJS.ProcessEnv;
  fs: FsLike;
  log?: (stage: string, msg: string, extra?: Record<string, unknown>) => void;
  disabled?: boolean;
  binary?: string;
}): RtkAdapter {
  if (!execFile) {
    throw new Error("createRtkAdapter: `execFile` is required");
  }
  if (!fs) {
    throw new Error("createRtkAdapter: `fs` is required");
  }

  let state: RtkState | null = null;
  let initPromise: Promise<RtkState> | null = null;

  const init = async (): Promise<RtkState> => {
    if (state) return state;
    if (initPromise) return initPromise;
    initPromise = (async (): Promise<RtkState> => {
      if (disabled) {
        state = { source: "raw", binary: null, reason: "disabled" };
        return state;
      }
      const resolved = await whichRtk(binary, execFile);
      if (!resolved) {
        state = { source: "raw", binary: null, reason: "binary-missing" };
        return state;
      }
      // Smoke test: a tiny `rtk read` against `/dev/null` exercises
      // the same code path the adapter's real calls use (read +
      // --truncate-lines-at + a path arg). On macOS the homebrew
      // rtk 0.42.3 internally shells out to bash's `read`
      // builtin (instead of rtk's own readline), which fails
      // on `--` with `read: --: invalid option`. Detecting the
      // failure at init() means every read goes straight to
      // fs.readFile and the operator sees one clean "rtk
      // disabled (smoke-test failed: ...)" log instead of the
      // same error after every file. The runner image ships
      // rtk 0.44.2 from the official release, which does not
      // have this bug — the smoke test is local-dev safety.
      if (!(await smokeTestRtk(resolved, execFile))) {
        state = {
          source: "raw",
          binary: resolved,
          reason: `smoke-test-failed: rtk at ${resolved} failed the read smoke test`,
        };
        return state;
      }
      state = { source: "rtk", binary: resolved, reason: null };
      return state;
    })();
    return initPromise;
  };

  const readFile = async (
    path: string,
    _encoding: string,
    options: { maxLines?: number; truncateLinesAt?: number } = {},
  ): Promise<string> => {
    const s = await init();
    if (s.source !== "rtk") {
      // Single info log on first fallback so an operator can tell
      // why their reads are not compressed; subsequent calls
      // skip the log so it doesn't drown the runner output.
      if (s.reason && !s.logged) {
        log("rtk", `falling back to raw read (${s.reason})`, {
          binary,
        });
        s.logged = true;
      }
      return readRaw(fs, path);
    }
    try {
      return await readViaRtk(execFile, env, s.binary!, path, options);
    } catch (err) {
      // First rtk call failure: flip the adapter to raw mode
      // for the rest of the run so the fallback log fires
      // once, not per file. The common case is a binary that
      // is on PATH (init() passed) but cannot actually read
      // the mount — e.g. the installed rtk rejects the
      // ConfigMap path with "No such file or directory".
      // Logging every failed call would produce 5+ noise
      // lines per run; logging once and switching to raw
      // mode is the same pattern init() already uses for
      // the binary-missing case.
      s.source = "raw";
      s.reason = `rtk-error: ${String(err instanceof Error ? err.message : err).split("\n")[0]}`;
      s.binary = null;
      log("rtk", "rtk read failed; switching to raw read for the rest of the run", {
        path,
        err: String(err instanceof Error ? err.message : err),
      });
      return readRaw(fs, path);
    }
  };

  const adapter: RtkAdapter = {
    readFile,
    init,
    get source() {
      return state ? state.source : "uninitialized";
    },
  };
  return adapter;
}
