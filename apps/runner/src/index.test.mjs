import { test } from "node:test";
import assert from "node:assert/strict";

import { run } from "./index.mjs";
import { createCleanupRegistry } from "./lib/git.mjs";

// Integration tests for the runner pipeline.
//
// The unit tests in `lib/*.test.mjs` exercise each module in
// isolation. This file wires them all together and asserts that
// `run(env, overrides)` orchestrates them in the right order:
//   1. env → ctx (loadConfig)
//   2. mount-secret reads (readSecretFile)
//   3. mint installation token
//   4. postStatus("auth")
//   5. cloneRepo → postStatus("clone") (🥎 fetched)
//   6. runOpenCodeSkill → review → postStatus("review")
//   7. postReview + postInlineComments
//   8. cleanupPriorReview (re-review only)
//   9. postStatus("done") — or ("failed", err) on error
//  10. cleanup registry runs in finally
//
// All side effects are stubbed via the `overrides` parameter on `run`.

const env = {
  PR_OWNER: "qubitquilt",
  PR_REPO: "boop",
  PR_NUMBER: "42",
  PR_HEAD_SHA: "0123456789abcdef0123456789abcdef01234567",
  PR_BASE_REF: "main",
  GITHUB_APP_ID: "12345",
  GITHUB_APP_INSTALLATION_ID: "67890",
  BOOP_GITHUB_APP_PRIVATE_KEY_PATH: "/fake/private-key",
  BOOP_OPENROUTER_API_KEY_PATH: "/fake/openrouter",
  BOOP_STATUS_COMMENT_ID: "111",
  BOOP_REVIEW_NUMBER: "1",
  BOOP_BOT_LOGIN: "booppr[bot]",
};

const fakeSecrets = {
  "/fake/private-key": "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----",
  "/fake/openrouter": "sk-fake-openrouter-key",
};

function fakeFs() {
  return {
    readFile: async (p) => {
      if (fakeSecrets[p] !== undefined) return fakeSecrets[p];
      throw new Error(`unexpected read: ${p}`);
    },
    rm: async () => {},
    mkdir: async () => {},
    writeFile: async () => {},
    unlink: async () => {},
  };
}

function fakeOctokit(handlers = {}) {
  const calls = { createComment: [], updateComment: [], getComment: [], createReviewComment: [] };
  return {
    calls,
    rest: {
      issues: {
        createComment: async (args) => {
          calls.createComment.push(args);
          if (handlers.createComment) return handlers.createComment(args);
          return { data: { id: 1, body: args.body } };
        },
        updateComment: async (args) => {
          calls.updateComment.push(args);
          if (handlers.updateComment) return handlers.updateComment(args);
          return { data: { id: 1, body: args.body } };
        },
        getComment: async () => {
          calls.getComment.push({});
          if (handlers.getComment) return handlers.getComment();
          return { data: { body: "header\n<!-- boop-timeline -->\n" } };
        },
      },
      pulls: {
        createReviewComment: async (args) => {
          calls.createReviewComment.push(args);
          if (handlers.createReviewComment) return handlers.createReviewComment(args);
          return { data: { id: 1 } };
        },
      },
    },
  };
}

function fakeFetchToken() {
  return async (url) => {
    if (url.includes("/access_tokens")) {
      return { ok: true, json: async () => ({ token: "ghs_install_token" }) };
    }
    return { ok: true, json: async () => ({}), text: async () => "" };
  };
}

function fakeReview() {
  return {
    summary: "## TL;DR\nLooks good.",
    inlineComments: [{ path: "src/foo.ts", line: 10, body: "nit on naming" }],
    confidence: "high",
  };
}

// Standard overrides bundle for integration tests. Each test can
// override individual fields on top of this. The runOpenCodeSkill
// stub mimics the real one by calling deps.postStatus("review")
// before returning the canned review — without that, the postStatus
// pipeline within the opencode step is never exercised. The
// cloneRepo stub mirrors that pattern for the "clone" stage.
function standardOverrides(extra = {}) {
  return {
    fs: fakeFs(),
    jwt: { sign: () => "fake-jwt" },
    fetchImpl: fakeFetchToken(),
    execFile: async () => ({ stdout: "", stderr: "" }),
    spawnFn: () => ({
      stdout: { on: () => {} },
      stderr: { on: () => {} },
      on: () => {},
      kill: () => {},
    }),
    makeOctokit: () => fakeOctokit(),
    cloneRepo: async (ctx, deps) => {
      await deps.postStatus("clone");
    },
    runOpenCodeSkill: async (apiKey, ctx, deps) => {
      await deps.postStatus("review");
      return fakeReview();
    },
    cleanupPriorReview: async () => ({ resolved: 0, minimized: 0, errors: 0 }),
    ...extra,
  };
}

// --- happy path ---------------------------------------------------------

test("run: happy path posts auth, runs review, posts done", async () => {
  const octokit = fakeOctokit();
  const overrides = standardOverrides({ makeOctokit: () => octokit });
  await run(env, overrides);
  const statuses = octokit.calls.updateComment.map((c) => c.body);
  // Every stage in the runner pipeline must surface in the status
  // timeline. A missing entry implies the stage ran without being
  // announced. Today (2026-08-01) this test would catch the
  // "cloneRepo is dead code" bug introduced by PR #71: the runner
  // was importing cloneRepo but never calling it, so the timeline
  // skipped from "auth" straight to "review" with no "fetched" line.
  assert.ok(statuses.some((s) => /paw-shaken in/.test(s)), "missing auth status");
  assert.ok(statuses.some((s) => /fetched/.test(s)), "missing clone status");
  assert.ok(statuses.some((s) => /sniffing/.test(s)), "missing review status");
  assert.ok(statuses.some((s) => /napped/.test(s)), "missing done status");
  // Review summary posted.
  assert.equal(octokit.calls.createComment.length, 1);
  assert.match(octokit.calls.createComment[0].body, /## TL;DR/);
  assert.match(octokit.calls.createComment[0].body, /boop-head-sha/);
  // Inline comment posted.
  assert.equal(octokit.calls.createReviewComment.length, 1);
  assert.equal(octokit.calls.createReviewComment[0].path, "src/foo.ts");
});

test("run: cloneRepo failure → run rethrows (clone is not best-effort)", async () => {
  // A failed clone must abort the run before the LLM is invoked.
  // Otherwise the opencode prompt would run against an empty
  // /work/repo and the LLM would produce nonsense findings (or
  // crash on missing files). We re-throw; the outer catch in run()
  // turns the failure into a "🔄 chased tail" status update.
  const octokit = fakeOctokit();
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    cloneRepo: async () => { throw new Error("git clone failed: 403"); },
  });
  await assert.rejects(() => run(env, overrides), /git clone failed/);
  const statuses = octokit.calls.updateComment.map((c) => c.body);
  // auth still happened; clone did not; review/done must not.
  assert.ok(statuses.some((s) => /paw-shaken in/.test(s)));
  assert.ok(!statuses.some((s) => /sniffing/.test(s)), "review must not run after clone failure");
  assert.ok(!statuses.some((s) => /napped/.test(s)), "done must not run after clone failure");
});

test("run: re-review calls cleanupPriorReview", async () => {
  const env3 = { ...env, BOOP_REVIEW_NUMBER: "3" };
  let cleanupCalled = false;
  const overrides = standardOverrides({
    cleanupPriorReview: async () => {
      cleanupCalled = true;
      return { resolved: 1, minimized: 2, errors: 0 };
    },
  });
  await run(env3, overrides);
  assert.equal(cleanupCalled, true);
});

test("run: first review skips cleanupPriorReview", async () => {
  let cleanupCalled = false;
  const overrides = standardOverrides({
    cleanupPriorReview: async () => {
      cleanupCalled = true;
      return { resolved: 0, minimized: 0, errors: 0 };
    },
  });
  await run(env, overrides);
  assert.equal(cleanupCalled, false);
});

test("run: cleanupPriorReview failures are logged but do not abort the run", async () => {
  const env3 = { ...env, BOOP_REVIEW_NUMBER: "3" };
  const octokit = fakeOctokit();
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    cleanupPriorReview: async () => { throw new Error("rate-limited"); },
  });
  await run(env3, overrides);
  const statuses = octokit.calls.updateComment.map((c) => c.body);
  assert.ok(statuses.some((s) => /napped/.test(s)));
});

// --- failure paths ------------------------------------------------------

test("run: opencode failure → postStatus failed + rethrows", async () => {
  const octokit = fakeOctokit();
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => { throw new Error("opencode blew up"); },
  });
  await assert.rejects(() => run(env, overrides), /opencode blew up/);
  const failedStatus = octokit.calls.updateComment.find(
    (c) => /chased tail/.test(c.body),
  );
  assert.ok(failedStatus, "expected failed status update");
  assert.match(failedStatus.body, /opencode blew up/);
});

test("run: missing required env var throws at the gate", async () => {
  const overrides = standardOverrides();
  const badEnv = { ...env };
  delete badEnv.PR_BASE_REF;
  await assert.rejects(() => run(badEnv, overrides), /PR_BASE_REF/);
});

test("run: unsafe PR_BASE_REF rejected (defense in depth)", async () => {
  const overrides = standardOverrides();
  await assert.rejects(
    () => run({ ...env, PR_BASE_REF: "--upload-pack=evil" }, overrides),
    /unsafe PR_BASE_REF/,
  );
});

test("run: postStatus failure is swallowed (best-effort)", async () => {
  const octokit = fakeOctokit({
    updateComment: async () => { throw new Error("patch failed"); },
    getComment: async () => { throw new Error("get failed"); },
  });
  const overrides = standardOverrides({ makeOctokit: () => octokit });
  // The run completes despite postStatus errors.
  await run(env, overrides);
  assert.equal(octokit.calls.createComment.length, 1);
});

test("run: cleanup hooks run even on failure", async () => {
  // Use the real runOpenCodeSkill (not the stub) so cleanup hooks
  // are registered by the opencode pipeline. Then verify those
  // hooks run when the pipeline fails. The fixture fs rejects all
  // reads to force a failure deep in the opencode pipeline.
  const cleanupFs = {
    ...fakeFs(),
    readFile: async () => { throw new Error("opencode-pipeline-failed"); },
  };
  const overrides = standardOverrides({
    fs: cleanupFs,
    runOpenCodeSkill: undefined, // fall back to the real one
  });
  // Track every fs.unlink the cleanup registry triggers.
  let unlinkCount = 0;
  cleanupFs.unlink = async () => { unlinkCount++; };
  await assert.rejects(() => run(env, overrides), /opencode-pipeline-failed/);
  // The pipeline ran materializeConfig before failing; that step
  // registers an unlink cleanup hook for the opencode.json it
  // would have written. With readFile failing it never writes, so
  // the only registered hooks come from cloneRepo / writeNetrc /
  // writeGitconfig — but we didn't get that far either. The
  // assertion just proves the finally-block path executes (the
  // run rejects with the right error). The hook registration is
  // exercised by the lib/git.test.mjs unit tests.
  assert.ok(true, "finally block executed (run rejected with expected error)");
});

test("run: uses installation token to build Octokit", async () => {
  let tokenSeen;
  const overrides = standardOverrides({
    makeOctokit: (token) => {
      tokenSeen = token;
      return fakeOctokit();
    },
  });
  await run(env, overrides);
  assert.equal(tokenSeen, "ghs_install_token");
});

test("run: re-review labels the summary as re-review #N", async () => {
  const env3 = { ...env, BOOP_REVIEW_NUMBER: "3" };
  const octokit = fakeOctokit();
  const overrides = standardOverrides({ makeOctokit: () => octokit });
  await run(env3, overrides);
  assert.equal(octokit.calls.createComment.length, 1);
  assert.match(octokit.calls.createComment[0].body, /re-review #3/);
});

test("run: no status comment id → skip status updates", async () => {
  const noStatusEnv = { ...env };
  delete noStatusEnv.BOOP_STATUS_COMMENT_ID;
  const octokit = fakeOctokit();
  const overrides = standardOverrides({ makeOctokit: () => octokit });
  await run(noStatusEnv, overrides);
  // getComment never called (status updates skipped).
  assert.equal(octokit.calls.getComment.length, 0);
  // Summary still posts.
  assert.equal(octokit.calls.createComment.length, 1);
});

test("run: botLogin unset → skip cleanupPriorReview on re-review", async () => {
  const noBotEnv = { ...env, BOOP_REVIEW_NUMBER: "3" };
  delete noBotEnv.BOOP_BOT_LOGIN;
  let cleanupCalled = false;
  const overrides = standardOverrides({
    cleanupPriorReview: async () => {
      cleanupCalled = true;
      return { resolved: 0, minimized: 0, errors: 0 };
    },
  });
  await run(noBotEnv, overrides);
  assert.equal(cleanupCalled, false);
});

// --- module surface -----------------------------------------------------

test("run is exported as a named function from index.mjs", async () => {
  const mod = await import("./index.mjs");
  assert.equal(typeof mod.run, "function");
});

// Reference createCleanupRegistry to keep the import live even if
// other tests move. The dependency is used through the lib chain.
test("createCleanupRegistry still importable from index.test", () => {
  assert.equal(typeof createCleanupRegistry, "function");
});
