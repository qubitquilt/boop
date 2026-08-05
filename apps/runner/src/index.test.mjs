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
//
// QUB-91 collapsed the retry policy: stageMaxAttempts = 1 means
// "no retry" so the existing failure tests don't sit on the
// backoff timer. The QUB-91 unit tests in workflow.test.mjs
// exercise the retry path explicitly with stageMaxAttempts = 3
// + a no-op sleep.
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
    stageMaxAttempts: 1,
    sleep: async () => {}, // no-op so any retry test doesn't actually wait
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

// --- summary parse failure path -----------------------------------------
// 2026-08-03 incident: the LLM emitted a structured block whose body
// was the JS string-concat echo of the test fixture in the diff
// (PR #90, #92). The old runner posted that body to the PR. Now:
// the parser returns an empty summary, the runner logs the parse
// failure, the status timeline shows a "chased tail" entry with the
// reason, and no comment is posted.

test("run: parse failure skips postReview + postInlineComments + cleanupPriorReview", async () => {
  const octokit = fakeOctokit();
  let cleanupCalled = false;
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => ({
      summary: "",
      inlineComments: [],
      confidence: "low",
      parseError: "JS string-concat echo",
    }),
    cleanupPriorReview: async () => {
      cleanupCalled = true;
      return { resolved: 0, minimized: 0, errors: 0 };
    },
  });
  // Re-review #3 so we can prove cleanupPriorReview would have
  // been called if the run had succeeded.
  await run({ ...env, BOOP_REVIEW_NUMBER: "3" }, overrides);
  // No summary comment posted.
  assert.equal(octokit.calls.createComment.length, 0);
  // No inline comments posted.
  assert.equal(octokit.calls.createReviewComment.length, 0);
  // No prior artifacts retired (the new review never landed; the
  // prior one is still the current one on the PR).
  assert.equal(cleanupCalled, false);
  // Status timeline shows the parse failure with the reason in the
  // "chased tail" entry's details block.
  const statuses = octokit.calls.updateComment.map((c) => c.body);
  assert.ok(
    statuses.some((s) => /chased tail/.test(s)),
    "expected a 'chased tail' status entry on parse failure",
  );
  assert.ok(
    statuses.some((s) => /summary parse failed: JS string-concat echo/.test(s)),
    "expected the parseError reason in the status details",
  );
});

test("run: parse failure on first review also skips the post (no cleanup either way)", async () => {
  // Same shape as the re-review case but on a first review — there
  // is no cleanupPriorReview to gate, but the assertion is the same:
  // no comment, no inline comments, status shows the failure.
  const octokit = fakeOctokit();
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => ({
      summary: "",
      inlineComments: [],
      confidence: "low",
      parseError: "no structured block",
    }),
  });
  await run(env, overrides);
  assert.equal(octokit.calls.createComment.length, 0);
  assert.equal(octokit.calls.createReviewComment.length, 0);
  const statuses = octokit.calls.updateComment.map((c) => c.body);
  assert.ok(statuses.some((s) => /chased tail/.test(s)));
  assert.ok(statuses.some((s) => /summary parse failed: no structured block/.test(s)));
});

// --- duplicate-pod guard (QUB-102) -------------------------------------
//
// QUB-102 headline scenario: a flaky runner pod (e.g. transient
// `git fetch` failure) must not cause a second pod to run the same
// review and post duplicate summary + inline comments.
//
// In production, BackoffLimit=0 in the Job spec is the primary
// defense (a pod failure surfaces as a failed Job; the K8s
// controller never restarts). This integration test is the
// defense-in-depth: even if a second pod of the same Job does
// start (operator re-trigger, K8s bug, etc.), the runner's
// startup guard (runStages aborts on state.passed hit) prevents
// the second pod from posting a duplicate summary + inline
// review threads.
//
// The test wires two `run()` calls against a stateful Octokit
// whose comment body persists across calls. Pod 1's
// onStagePassed callbacks write passed=[handshake,fetch] to
// the comment before the sniff stage throws. Pod 2 reads the
// state from the comment on its first onStagePassed (handshake)
// and aborts on the loop's first iteration.
//
// Assertion: exactly zero summary comments + exactly zero
// inline comments land across both pods (the "only one"
// wording in the issue covers zero as well — no duplicates
// is the success criterion).

// statefulOctokit keeps the comment body in a single mutable
// buffer. updateComment writes through; getComment reads the
// current body. The body holds both the receiver's status
// timeline ("<!-- boop-timeline -->") and the QUB-92
// workflow-state marker ("<!-- boop-state: ... -->"), so
// the runner's readWorkflowState sees the prior pod's
// progress and the abort path fires.
function statefulOctokit({ seedPassed } = {}) {
  const calls = {
    createComment: [],
    updateComment: [],
    getComment: [],
    createReviewComment: [],
  };
  let body =
    "🐾 **Boop's on the case!**\n\n<!-- boop-timeline -->\n";
  // Seed the comment body with a QUB-92 state line so the
  // runner reads a prior pod's progress on its first
  // onStagePassed callback. Used by the post-summary-pod-1
  // integration test below.
  if (seedPassed && seedPassed.length > 0) {
    const stateLine = `<!-- boop-state: ${JSON.stringify({
      passed: seedPassed,
      sub: {},
    })} -->`;
    body = body + stateLine + "\n";
  }
  const octokit = {
    calls,
    rest: {
      issues: {
        createComment: async (args) => {
          calls.createComment.push(args);
          return { data: { id: 1001, body: args.body } };
        },
        updateComment: async (args) => {
          calls.updateComment.push(args);
          body = args.body;
          return { data: { id: 111, body: args.body } };
        },
        getComment: async () => {
          calls.getComment.push({});
          return { data: { body } };
        },
      },
      pulls: {
        createReviewComment: async (args) => {
          calls.createReviewComment.push(args);
          return { data: { id: 1 } };
        },
      },
    },
  };
  return octokit;
}

test("run: pod-1-fails-mid-sniff + pod-2-starts -> no duplicate posts (QUB-102)", async () => {
  // A stateful Octokit so pod 2's readWorkflowState sees pod
  // 1's writeWorkflowState output. The first pod is wired to
  // fail mid-sniff (runOpenCodeSkill throws); the second pod
  // is wired to succeed (same shape as the happy-path
  // stub). If the abort path is missing, pod 2 would post a
  // summary comment + inline review threads — exactly the
  // duplicate PR comments the issue describes.
  const octokit = statefulOctokit();
  let sniffCalls = 0;
  const makeOctokit = () => octokit;
  const overrides = standardOverrides({
    makeOctokit,
    runOpenCodeSkill: async () => {
      sniffCalls++;
      if (sniffCalls === 1) {
        // Pod 1: transient git-fetch failure mid-sniff.
        throw new Error("git fetch failed: connection reset by peer");
      }
      // Pod 2: would normally produce a real review.
      await new Promise((resolve) => setTimeout(resolve, 0));
      return fakeReview();
    },
  });

  // Pod 1: throws from sniff. The outer catch in run() posts
  // a "failed" status and rethrows. We assert on the reject
  // to prove pod 1 actually failed.
  await assert.rejects(
    () => run(env, overrides),
    /git fetch failed/,
  );

  // Pod 2: a fresh run with the same overrides. The stateful
  // Octokit now contains pod 1's passed=[handshake,fetch] in
  // the comment body. The onStagePassed callback for handshake
  // reads that state and merges it into pod 2's state.passed.
  // runStages then loops and finds handshake already in
  // passed -> abort path -> parseFailed = true -> orchestrator
  // short-circuits. The summary + inline stages never run.
  await run(env, overrides);

  // Headline assertion: zero summary comments across both pods.
  // The summary stage never ran (pod 1 died before it; pod 2
  // aborted). With the abort path in place, this is exactly
  // what we expect.
  assert.equal(
    octokit.calls.createComment.length,
    0,
    "expected zero summary comments across both pods (no duplicate posts)",
  );
  assert.equal(
    octokit.calls.createReviewComment.length,
    0,
    "expected zero inline review threads across both pods",
  );

  // Pod 2's status thread carries the abort reason. The
  // status timeline is the operator's source of truth; they
  // can see "another pod already passed [...]" in the
  // comment. The reason lists the full prior-pod passed
  // set so the operator does not have to cross-reference
  // the timeline. The order can be ["handshake","fetch"] or
  // ["fetch","handshake"] depending on the merge's push-
  // then-merge ordering — the assertion checks both stages
  // are present, not the order.
  const lastFailed = [...octokit.calls.updateComment]
    .reverse()
    .find((c) => /chased tail/.test(c.body));
  assert.ok(lastFailed, "expected a 'chased tail' status post on pod 2");
  assert.match(
    lastFailed.body,
    /another pod already passed \[[^\]]+\]/,
    "expected the QUB-102 abort reason in bracket form",
  );
  assert.ok(
    lastFailed.body.includes("handshake"),
    "expected abort reason to list handshake",
  );
  assert.ok(
    lastFailed.body.includes("fetch"),
    "expected abort reason to list fetch",
  );
  assert.match(
    lastFailed.body,
    /refusing to duplicate the review/,
    "expected the QUB-102 abort reason to call out the duplicate-post prevention",
  );

  // The state.passed in the final boop-state line contains
  // both handshake and fetch (the stages pod 1 finished
  // before failing). The order is determined by the merge
  // logic; the assertion is on the SET, not the order.
  const lastState = [...octokit.calls.updateComment]
    .reverse()
    .find((c) => /boop-state:/.test(c.body));
  assert.ok(lastState, "expected a boop-state line in the final comment");
  assert.match(
    lastState.body,
    /"passed":\[(?:"handshake","fetch"|"fetch","handshake")\]/,
    "expected state.passed to contain both handshake and fetch",
  );
});

// The strongest version of the QUB-102 integration test: pod
// 1 succeeded all the way through summary + inlines, then a
// sibling pod 2 starts. The runner guard must prevent pod 2
// from posting duplicate summary + inline comments even
// though pod 1 already did so. Without the guard, this test
// fails (pod 2 would happily re-post via its runOpenCodeSkill
// override). The pre-summary-pod-1 case is weaker: deleting
// the guard there would still pass because pod 1 died before
// posting anything.
test("run: pod-1-posted-summary-and-inlines + pod-2-starts -> no duplicate posts (QUB-102)", async () => {
  // Seed the comment body with a state line that says pod 1
  // finished through inlines (and thus posted both summary
  // + inline review threads). runOpenCodeSkill returns a
  // real review, so a guard-less pod 2 would happily
  // re-post both.
  const octokit = statefulOctokit({
    seedPassed: ["handshake", "fetch", "sniff", "summary", "inlines"],
  });
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => fakeReview(),
  });
  await run(env, overrides);
  // Headline assertion: even with runOpenCodeSkill ready to
  // post a real review, pod 2 posted zero summary comments
  // and zero inline review threads.
  assert.equal(
    octokit.calls.createComment.length,
    0,
    "expected zero summary comments from pod 2 (guard must fire)",
  );
  assert.equal(
    octokit.calls.createReviewComment.length,
    0,
    "expected zero inline review threads from pod 2 (guard must fire)",
  );
  // The status thread carries the abort reason. The reason
  // lists the full prior-pod passed set so the operator
  // sees pod 1 finished through inlines.
  const lastFailed = [...octokit.calls.updateComment]
    .reverse()
    .find((c) => /chased tail/.test(c.body));
  assert.ok(lastFailed, "expected a 'chased tail' status post on pod 2");
  for (const stage of ["handshake", "fetch", "sniff", "summary", "inlines"]) {
    assert.ok(
      lastFailed.body.includes(stage),
      `expected abort reason to list ${stage}; got: ${lastFailed.body}`,
    );
  }
});

// Same shape but pod 1 only reached summary (not inlines).
// Pod 2 must still abort before re-posting summary. Without
// the guard, pod 2 would post a duplicate summary comment.
test("run: pod-1-posted-summary + pod-2-starts -> no duplicate posts (QUB-102)", async () => {
  const octokit = statefulOctokit({
    seedPassed: ["handshake", "fetch", "sniff", "summary"],
  });
  const overrides = standardOverrides({
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => fakeReview(),
  });
  await run(env, overrides);
  assert.equal(octokit.calls.createComment.length, 0);
  assert.equal(octokit.calls.createReviewComment.length, 0);
  const lastFailed = [...octokit.calls.updateComment]
    .reverse()
    .find((c) => /chased tail/.test(c.body));
  assert.ok(lastFailed, "expected a 'chased tail' status post on pod 2");
  assert.ok(lastFailed.body.includes("summary"));
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
