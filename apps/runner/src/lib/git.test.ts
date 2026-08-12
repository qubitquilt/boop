import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, realpathSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { createCleanupRegistry, cloneRepo } from "./git.ts";

const PATHS = {
  repoDir: "/work/repo",
  // configSrc is the runner-config ConfigMap mount — kept
  // because buildBoopPrompt reads skill files from there. The
  // writable opencode-* paths and the configDir/materializeConfig
  // setup are gone (QUB-98).
  configSrc: "/home/opencode/.config/opencode",
  netrc: "/tmp/boop-netrc",
  gitconfig: "/tmp/boop-gitconfig",
};

// --- CleanupRegistry ----------------------------------------------------

test("CleanupRegistry runs hooks in parallel and swallows failures", async () => {
  const calls = [];
  const reg = createCleanupRegistry({ errlog: () => {} });
  reg.register(async () => { calls.push("a"); });
  reg.register(async () => { calls.push("b"); throw new Error("boom"); });
  reg.register(async () => { calls.push("c"); });
  await reg.runAll();
  // All three were invoked despite the middle one failing.
  assert.deepEqual(calls.sort(), ["a", "b", "c"]);
});

test("CleanupRegistry.runAll is idempotent (does not re-run hooks)", async () => {
  const reg = createCleanupRegistry({ errlog: () => {} });
  let runs = 0;
  reg.register(async () => { runs++; });
  await reg.runAll();
  await reg.runAll();
  assert.equal(runs, 1);
});

test("CleanupRegistry surfaces failures via errlog", async () => {
  const logs = [];
  const reg = createCleanupRegistry({
    errlog: (stage, msg, extra) => logs.push({ stage, msg, extra }),
  });
  reg.register(async () => { throw new Error("kaboom"); });
  await reg.runAll();
  assert.equal(logs.length, 1);
  assert.equal(logs[0].stage, "cleanup");
  assert.equal(logs[0].msg, "cleanup step failed");
  assert.match(logs[0].extra.err, /kaboom/);
});

// --- cloneRepo ----------------------------------------------------------

function makeMockFs() {
  const calls = [];
  return {
    calls,
    rm: async (...args) => { calls.push(["rm", args]); },
    mkdir: async (...args) => { calls.push(["mkdir", args]); },
    writeFile: async (...args) => { calls.push(["writeFile", args]); },
    unlink: async (...args) => { calls.push(["unlink", args]); },
  };
}

function makeMockExecFile() {
  const calls = [];
  const fn = async (...args) => { calls.push(args); return { stdout: "", stderr: "" }; };
  fn.calls = calls;
  return fn;
}

function makeCtx(overrides = {}) {
  return {
    prOwner: "qubitquilt",
    prRepo: "boop",
    prNumber: "42",
    prHeadSha: "0123456789abcdef0123456789abcdef01234567",
    prBaseRef: "main",
    previousHeadSha: null,
    installationToken: "ghs_testtoken",
    home: "/home/opencode",
    ...overrides,
  };
}

function makeDeps(overrides = {}) {
  const cleanup = createCleanupRegistry({ errlog: () => {} });
  return {
    fs: makeMockFs(),
    execFile: makeMockExecFile(),
    paths: PATHS,
    cleanup,
    log: () => {},
    ...overrides,
  };
}

test("cloneRepo runs clone, fetch, and checkout in order", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx(), deps);
  assert.equal(deps.execFile.calls.length, 3);
  // Each invocation's first arg is the binary ("git").
  for (const call of deps.execFile.calls) {
    assert.equal(call[0], "git");
  }
  // Args are arrays starting at index 2.
  const op = (i) => deps.execFile.calls[i][1];
  assert.ok(op(0).includes("clone"));
  assert.ok(op(1).includes("fetch"));
  assert.ok(op(2).includes("checkout"));
});

test("cloneRepo uses `git fetch origin -- <refs>` (positional refs after `--`)", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx(), deps);
  const fetchArgs = deps.execFile.calls[1][1];
  const sepIdx = fetchArgs.indexOf("--");
  assert.ok(sepIdx > -1, "expected `--` separator in fetch args");
  assert.deepEqual(fetchArgs.slice(sepIdx + 1), ["main", "0123456789abcdef0123456789abcdef01234567"]);
});

test("cloneRepo includes previousHeadSHA in fetch when set", async () => {
  const deps = makeDeps();
  await cloneRepo(
    makeCtx({ previousHeadSha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }),
    deps,
  );
  const fetchArgs = deps.execFile.calls[1][1];
  const sepIdx = fetchArgs.indexOf("--");
  assert.deepEqual(
    fetchArgs.slice(sepIdx + 1),
    ["main", "0123456789abcdef0123456789abcdef01234567", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],
  );
});

test("cloneRepo checks out the head SHA", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx(), deps);
  const checkoutArgs = deps.execFile.calls[2][1];
  // The head SHA must be the positional argument to `git checkout`.
  // Do NOT use `--` between `checkout` and the SHA: `git checkout -- <pathspec>`
  // interprets the SHA as a filesystem path, not a commit ref. The
  // checkout command is therefore `git checkout <sha>` — no `--`.
  assert.deepEqual(checkoutArgs, [
    "checkout",
    "0123456789abcdef0123456789abcdef01234567",
  ]);
});

test("cloneRepo uses HOME from ctx (not process env)", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx({ home: "/custom-home" }), deps);
  for (const call of deps.execFile.calls) {
    assert.equal(call[2].env.HOME, "/custom-home");
  }
});

test("cloneRepo sets GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM on the env", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx(), deps);
  for (const call of deps.execFile.calls) {
    assert.equal(call[2].env.GIT_CONFIG_GLOBAL, PATHS.gitconfig);
    assert.equal(call[2].env.GIT_CONFIG_NOSYSTEM, "1");
  }
});

test("cloneRepo writes netrc and gitconfig in mode 0600", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx(), deps);
  const writes = deps.fs.calls.filter((c) => c[0] === "writeFile");
  assert.equal(writes.length, 2);
  // writeFile signature is fs.writeFile(path, data, options) —
  // args[0] is path, args[1] is body, args[2] is options.
  for (const w of writes) {
    assert.equal(w[1][2].mode, 0o600, `expected mode 0600, got ${JSON.stringify(w[1][2])}`);
  }
  const paths = writes.map((w) => w[1][0]).sort();
  assert.deepEqual(paths, [PATHS.gitconfig, PATHS.netrc]);
});

test("cloneRepo netrc body contains the installation token and github.com machine", async () => {
  const deps = makeDeps();
  await cloneRepo(makeCtx({ installationToken: "tok_xyz" }), deps);
  const netrcWrite = deps.fs.calls.find(
    (c) => c[0] === "writeFile" && c[1][0] === PATHS.netrc,
  );
  const body = netrcWrite[1][1];
  assert.match(body, /machine github\.com/);
  assert.match(body, /login x-access-token/);
  assert.match(body, /password tok_xyz/);
});

test("cloneRepo registers netrc + gitconfig + repoDir cleanup hooks", async () => {
  const cleanup = createCleanupRegistry({ errlog: () => {} });
  const unlinkCalls = [];
  const deps = {
    fs: {
      ...makeMockFs(),
      unlink: async (...args) => { unlinkCalls.push(args); },
    },
    execFile: makeMockExecFile(),
    paths: PATHS,
    cleanup,
    log: () => {},
  };
  await cloneRepo(makeCtx(), deps);
  await cleanup.runAll();
  // Two unlinks registered (netrc + gitconfig); the repo dir rm was
  // called explicitly at the top, not via the cleanup registry.
  assert.equal(unlinkCalls.length, 2);
  assert.deepEqual(unlinkCalls.map((c) => c[0]).sort(), [PATHS.gitconfig, PATHS.netrc]);
});

test("cloneRepo rejects unsafe refnames in ctx (defense in depth)", async () => {
  const deps = makeDeps();
  await assert.rejects(
    () => cloneRepo(makeCtx({ prBaseRef: "--upload-pack=evil" }), deps),
    /unsafe PR_BASE_REF/,
  );
});

test("cloneRepo rejects unsafe head SHA", async () => {
  const deps = makeDeps();
  await assert.rejects(
    () => cloneRepo(makeCtx({ prHeadSha: "not-a-sha" }), deps),
    /unsafe PR_HEAD_SHA/,
  );
});

// --- E2E credential helper regression (QUB-116) ------------------------

test("credential.helper from gitconfig is active without -c override (regression: QUB-116)", async (t) => {
  const tmp = realpathSync(mkdtempSync(join(tmpdir(), "qub-116-")));
  t.after(() => rmSync(tmp, { recursive: true, force: true }));

  const gitconfig = join(tmp, "gitconfig");
  const netrc = join(tmp, "netrc");

  writeFileSync(gitconfig, `[credential]\n\thelper = store --file=${netrc}\n`, "utf8");
  writeFileSync(netrc, "https://x-access-token:ghs_magic@github.com\n", "utf8");

  const env = {
    ...process.env,
    GIT_CONFIG_GLOBAL: gitconfig,
    GIT_CONFIG_NOSYSTEM: "1",
    HOME: tmp,
  };

  // Without `-c credential.helper=`, git must find the store helper and
  // return the stored credentials from the gitconfig.
  const ok = execFileSync("git", ["credential", "fill"], {
    env,
    input: "url=https://github.com\n\n",
    encoding: "utf8",
  });
  assert.match(ok, /password=ghs_magic/, "gitconfig helper must be active without -c override");

  // With `-c credential.helper=`, git must suppress the gitconfig helper
  // and fail when it falls through to terminal prompting.
  assert.throws(
    () => execFileSync("git", ["-c", "credential.helper=", "credential", "fill"], {
      env,
      input: "url=https://github.com\n\n",
      encoding: "utf8",
    }),
    /could not read/i,
    "-c credential.helper= must suppress the gitconfig helper",
  );
});
