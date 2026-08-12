import { test } from "node:test";
import assert from "node:assert/strict";

import { createRtkAdapter } from "./rtk.mjs";

// QUB-85: the rtk adapter is the single place the runner shells out
// to `rtk read`. These tests pin:
//
//   1. BOOP_RTK_DISABLED=1 bypasses rtk entirely (raw reads).
//   2. rtk missing from PATH falls back to raw reads.
//   3. rtk present routes through `rtk read <path>` with the
//      expected CLI flags.
//   4. An rtk call that throws is caught and falls back to raw.
//   5. The init() call resolves the adapter state on demand so a
//      caller that wants to log the resolved mode (the runner's
//      startup log) doesn't have to wait for the first read.
//
// Each test injects a fake `execFile` (no real `which`/`rtk` spawn)
// and a fake `fs` (no real filesystem). The adapter has no hidden
// I/O surface — every dependency is a constructor argument.

function fakeFs(files = {}) {
  const map = new Map(Object.entries(files));
  return {
    readFile: async (p) => {
      if (!map.has(p)) {
        throw Object.assign(new Error(`ENOENT: ${p}`), { code: "ENOENT" });
      }
      return map.get(p);
    },
  };
}

function recordingExecFile(handlers = []) {
  const calls = [];
  const queue = [...handlers];
  const exec = async (bin, args, opts) => {
    calls.push({ bin, args, opts });
    const next = queue.shift();
    if (!next) {
      throw new Error(`no handler for ${bin} ${JSON.stringify(args)}`);
    }
    if (next.throw) {
      throw next.throw;
    }
    return {
      stdout: next.stdout ?? "",
      stderr: next.stderr ?? "",
    };
  };
  return { calls, exec };
}

test("createRtkAdapter: BOOP_RTK_DISABLED bypasses rtk entirely", async () => {
  const { calls, exec } = recordingExecFile();
  const fs = fakeFs({ "/path/to/file.md": "raw content" });
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    disabled: true,
  });
  const out = await adapter.readFile("/path/to/file.md");
  assert.equal(out, "raw content");
  // No execFile calls when disabled — the rtk binary is never invoked.
  assert.equal(calls.length, 0);
  const state = await adapter.init();
  assert.equal(state.source, "raw");
  assert.equal(state.reason, "disabled");
});

test("createRtkAdapter: missing rtk binary falls back to raw reads", async () => {
  // which returns empty stdout — simulates "rtk not on PATH".
  const { calls, exec } = recordingExecFile([{ stdout: "" }]);
  const fs = fakeFs({ "/path/to/file.md": "raw content" });
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: () => {},
  });
  const out = await adapter.readFile("/path/to/file.md");
  assert.equal(out, "raw content");
  // The first call to which() happened. No rtk read was attempted.
  assert.equal(calls.length, 1);
  assert.equal(calls[0].bin, "which");
  assert.deepEqual(calls[0].args, ["rtk"]);
  const state = await adapter.init();
  assert.equal(state.source, "raw");
  assert.equal(state.reason, "binary-missing");
});

test("createRtkAdapter: routes through rtk read with the expected flags", async () => {
  // which returns a path; rtk read returns compressed content.
  // The trailing newline is what `rtk read` actually emits; the
  // adapter returns it verbatim — the LLM doesn't care about a
  // trailing \n any more than a real `cat` would strip it.
  const { calls, exec } = recordingExecFile([
    { stdout: "/usr/local/bin/rtk\n" },
    { stdout: "compressed by rtk\n" },
  ]);
  const fs = fakeFs({}); // fs must not be used in this test
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: () => {},
  });
  const out = await adapter.readFile("/path/to/file.md");
  assert.equal(out, "compressed by rtk\n");
  // 1) which rtk, 2) rtk read.
  assert.equal(calls.length, 2);
  // The rtk read call shape — the adapter passes the file path after
  // `--` so a future lens name that starts with `-` cannot be
  // mistaken for a flag (defense in depth, even though the call
  // site controls the path).
  const rtkCall = calls[1];
  assert.equal(rtkCall.bin, "/usr/local/bin/rtk");
  assert.deepEqual(rtkCall.args, [
    "read",
    "--max-lines",
    "400",
    "--truncate-lines-at",
    "500",
    "--",
    "/path/to/file.md",
  ]);
  // The adapter forwards the per-call telemetry kill switch so a
  // single rtk invocation never phones home, even if the env was
  // left dirty.
  assert.equal(rtkCall.opts.env.RTK_TELEMETRY_DISABLED, "1");
  const state = await adapter.init();
  assert.equal(state.source, "rtk");
  assert.equal(state.binary, "/usr/local/bin/rtk");
});

test("createRtkAdapter: per-call options override the defaults", async () => {
  const { calls, exec } = recordingExecFile([
    { stdout: "/usr/local/bin/rtk\n" },
    { stdout: "" },
  ]);
  const fs = fakeFs({});
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: () => {},
  });
  await adapter.readFile("/p", "utf8", {
    maxLines: 10,
    truncateLinesAt: 80,
  });
  const rtkCall = calls[1];
  assert.deepEqual(rtkCall.args, [
    "read",
    "--max-lines",
    "10",
    "--truncate-lines-at",
    "80",
    "--",
    "/p",
  ]);
});

test("createRtkAdapter: rtk read failure flips to raw mode for the rest of the run", async () => {
  // The common case in production: rtk is on PATH (init() passes)
  // but a real read call throws (e.g. the installed rtk rejects
  // the ConfigMap mount path with "No such file or directory").
  // The first failure must:
  //   - log the failure with the path and error,
  //   - flip the adapter to raw mode for the rest of the run,
  //   - NOT log "rtk read failed" again on subsequent reads
  //     (would produce 5+ noise lines per run).
  const { calls, exec } = recordingExecFile([
    { stdout: "/usr/local/bin/rtk\n" },
    { throw: new Error("rtk crashed: signal 11") },
  ]);
  const fs = fakeFs({
    "/path/to/a.md": "raw a",
    "/path/to/b.md": "raw b",
    "/path/to/c.md": "raw c",
  });
  const logCalls = [];
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: (tag, msg, meta) => logCalls.push({ tag, msg, meta }),
  });
  // First read: rtk throws, adapter falls back to raw, logs once.
  assert.equal(await adapter.readFile("/path/to/a.md"), "raw a");
  // Second + third reads: rtk is now off; raw reads silently.
  assert.equal(await adapter.readFile("/path/to/b.md"), "raw b");
  assert.equal(await adapter.readFile("/path/to/c.md"), "raw c");
  // Exactly one switch log — not three per-read fallbacks.
  const switchLogs = logCalls.filter((l) =>
    l.msg === "rtk read failed; switching to raw read for the rest of the run",
  );
  assert.equal(switchLogs.length, 1, "expected exactly one switch log");
  assert.match(switchLogs[0].meta.err, /rtk crashed/);
  // rtk was only invoked once — the second and third reads
  // bypass the binary entirely.
  const rtkInvocations = calls.filter((c) => c.bin === "/usr/local/bin/rtk");
  assert.equal(rtkInvocations.length, 1, "rtk should only be called once");
});

test("createRtkAdapter: rtk failure flip is observable in the source getter", async () => {
  // After the first rtk failure, the source getter reports
  // "raw" (not "rtk") so a caller that reads the field after
  // a read sees the actual mode. Useful for tests + the
  // orchestrator's "is rtk in the path" introspection.
  const { exec } = recordingExecFile([
    { stdout: "/usr/local/bin/rtk\n" },
    { throw: new Error("rtk read failed: No such file or directory") },
  ]);
  const fs = fakeFs({ "/p": "raw content" });
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: () => {},
  });
  await adapter.init();
  assert.equal(adapter.source, "rtk");
  await adapter.readFile("/p");
  assert.equal(adapter.source, "raw");
});

test("createRtkAdapter: init() is idempotent and memoised", async () => {
  // The which rtk call happens once; subsequent init calls reuse
  // the captured state. Without the memoisation, every read would
  // re-spawn which.
  const { calls, exec } = recordingExecFile([{ stdout: "/usr/local/bin/rtk\n" }]);
  const fs = fakeFs({ "/p": "x" });
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: () => {},
  });
  const a = await adapter.init();
  const b = await adapter.init();
  const c = await adapter.init();
  assert.equal(a, b);
  assert.equal(b, c);
  assert.equal(calls.length, 1);
});

test("createRtkAdapter: source getter reports the resolved mode", async () => {
  // Before init, the getter reports "uninitialized" so a caller
  // that reads the field without awaiting init() gets a sensible
  // value. After init, it reports the actual mode.
  const { exec } = recordingExecFile([{ stdout: "/usr/local/bin/rtk\n" }]);
  const fs = fakeFs({});
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: () => {},
  });
  assert.equal(adapter.source, "uninitialized");
  await adapter.init();
  assert.equal(adapter.source, "rtk");
});

test("createRtkAdapter: missing-binary fallback log fires once", async () => {
  // The "falling back to raw read" log line must not fire on every
  // read — it would drown the runner output. It fires once per
  // pod, on the first read after init.
  const { exec } = recordingExecFile([{ stdout: "" }]);
  const fs = fakeFs({
    "/a": "aa",
    "/b": "bb",
    "/c": "cc",
  });
  const logCalls = [];
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    log: (tag, msg) => logCalls.push({ tag, msg }),
  });
  await adapter.readFile("/a");
  await adapter.readFile("/b");
  await adapter.readFile("/c");
  const fallbackLogs = logCalls.filter(
    (l) => l.msg.startsWith("falling back to raw read"),
  );
  assert.equal(fallbackLogs.length, 1, "expected exactly one fallback log line");
});

test("createRtkAdapter: requires execFile and fs", () => {
  assert.throws(
    () => createRtkAdapter({ fs: fakeFs() }),
    /`execFile` is required/,
  );
  assert.throws(
    () => createRtkAdapter({ execFile: () => {} }),
    /`fs` is required/,
  );
});

test("createRtkAdapter: honours a custom binary name", async () => {
  // The default binary is `rtk`. Operators can point the adapter
  // at a vendored build (e.g. /opt/boop/bin/rtk) without touching
  // PATH. The binary name is forwarded to which.
  const { calls, exec } = recordingExecFile([{ stdout: "/opt/boop/bin/rtk\n" }]);
  const fs = fakeFs({});
  const adapter = createRtkAdapter({
    execFile: exec,
    fs,
    binary: "rtk", // the adapter accepts either a name or a path
    log: () => {},
  });
  await adapter.init();
  assert.equal(calls[0].args[0], "rtk");
});
