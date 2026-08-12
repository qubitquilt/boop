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

// Default binary name. Resolved once on the first call so a
// "command not found" surfaces as a clean ENOENT-shape error, not
// a multi-minute binary-search through PATH.
const DEFAULT_RTK_BIN = "rtk";

// Default max output line cap. Picked to keep the prompt context
// bounded even for a 10K-LOC file; the per-call adapter call can
// override this. The TOML filter in apps/runner/rtk/filters.toml
// re-applies the same cap as a defense in depth (a future caller
// that forgets to pass `--max-lines` still gets bounded output).
const DEFAULT_MAX_LINES = 400;

// Default per-line truncation. rtk's read accepts no per-line cap
// at the CLI level; we keep the value conservative so a single
// minified line cannot dominate the prompt.
const DEFAULT_TRUNCATE_LINES_AT = 500;

// whichRtk resolves the rtk binary path on PATH. Returns the
// absolute path or null. The check runs once per process via the
// `createRtkAdapter` memoization; subsequent reads reuse the
// captured result.
async function whichRtk(bin, execFile) {
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

// readRaw is the fallback. Same signature as fs.readFile, no rtk
// involvement. Used when rtk is disabled or the binary is missing.
async function readRaw(fs, path) {
  return fs.readFile(path, "utf8");
}

// readViaRtk shells out to `rtk read <path> --max-lines N
// --truncate-lines-at N` and returns the stdout. The adapter never
// reads stderr: rtk's stderr is diagnostic (tee file paths, the
// trust notice) and the LLM does not need it. If rtk exits
// non-zero, the caller gets the underlying execFile error.
async function readViaRtk(execFile, env, binary, path, options) {
  const maxLines = options.maxLines ?? DEFAULT_MAX_LINES;
  const truncateLinesAt = options.truncateLinesAt ?? DEFAULT_TRUNCATE_LINES_AT;
  const args = [
    "read",
    "--max-lines",
    String(maxLines),
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
    },
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
} = {}) {
  if (!execFile) {
    throw new Error("createRtkAdapter: `execFile` is required");
  }
  if (!fs) {
    throw new Error("createRtkAdapter: `fs` is required");
  }

  let state;
  let initPromise = null;

  const init = async () => {
    if (state) return state;
    if (initPromise) return initPromise;
    initPromise = (async () => {
      if (disabled) {
        state = { source: "raw", binary: null, reason: "disabled" };
        return state;
      }
      const resolved = await whichRtk(binary, execFile);
      if (!resolved) {
        state = { source: "raw", binary: null, reason: "binary-missing" };
        return state;
      }
      state = { source: "rtk", binary: resolved, reason: null };
      return state;
    })();
    return initPromise;
  };

  const readFile = async (path, _encoding, options = {}) => {
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
      return await readViaRtk(execFile, env, s.binary, path, options);
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
      s.reason = `rtk-error: ${String(err?.message ?? err).split("\n")[0]}`;
      s.binary = null;
      log("rtk", "rtk read failed; switching to raw read for the rest of the run", {
        path,
        err: String(err?.message ?? err),
      });
      return readRaw(fs, path);
    }
  };

  return {
    readFile,
    // Surface the resolved state to tests and to the orchestrator
    // (so a single startup log can confirm the adapter is in the
    // expected mode without waiting for the first read).
    init,
    get source() {
      return state ? state.source : "uninitialized";
    },
  };
}
