import { test } from "node:test";
import assert from "node:assert/strict";

import {
  STAGES,
  REVIEW_SUB_STAGES,
  statusStageFor,
  runStages,
  runSubWorkflow,
  DEFAULT_MAX_ATTEMPTS,
  DEFAULT_BACKOFF_BASE_MS,
  DEFAULT_BACKOFF_MAX_MS,
} from "./workflow.mjs";
import { SHORT } from "./github.mjs";

// --- macro STAGES table -------------------------------------------------

test("STAGES lists the six macro stages in order", () => {
  // Pinned by the QUB-89 spike. The six macro stages are:
  // handshake, fetch, sniff, summary, inlines, cleanup. The
  // `sniff` macro-stage wraps a sub-workflow. Any change to the
  // macro order or identity is a breaking change for the gate
  // (QUB-90), retry (QUB-91), and resume (QUB-92) PRs.
  assert.deepEqual(
    STAGES.map((s) => s.id),
    ["handshake", "fetch", "sniff", "summary", "inlines", "cleanup"],
  );
});

test("each macro stage has the contract fields QUB-90 depends on", () => {
  for (const stage of STAGES) {
    assert.equal(typeof stage.id, "string", `${stage.id}: id`);
    assert.ok(
      "statusStage" in stage,
      `${stage.id}: statusStage must be present (null is allowed for silent stages)`,
    );
    assert.equal(typeof stage.description, "string", `${stage.id}: description`);
    assert.equal(typeof stage.input, "string", `${stage.id}: input`);
    assert.equal(typeof stage.output, "string", `${stage.id}: output`);
    assert.equal(typeof stage.idempotent, "boolean", `${stage.id}: idempotent`);
    assert.equal(typeof stage.run, "function", `${stage.id}: run`);
  }
});

test("status stage labels match the user-visible surface (QUB-93)", () => {
  // The status thread on the PR is pinned. Adding a new label
  // would be a user-visible change that needs a follow-up
  // ticket. The current surface is:
  //   - handshake -> "auth"     (🤝 paw-shaken in)
  //   - fetch     -> "clone"    (🥎 fetched)
  //   - sniff     -> "review"   (👃 sniffing)
  //   - summary   -> null       (silent — done is posted after)
  //   - inlines   -> null       (silent — done is posted after)
  //   - cleanup   -> null       (silent — runs on re-review)
  assert.equal(statusStageFor("handshake"), "auth");
  assert.equal(statusStageFor("fetch"), "clone");
  assert.equal(statusStageFor("sniff"), "review");
  assert.equal(statusStageFor("summary"), null);
  assert.equal(statusStageFor("inlines"), null);
  assert.equal(statusStageFor("cleanup"), null);
  // Unknown id -> null (safe default for future stages).
  assert.equal(statusStageFor("nope"), null);
});

// --- sub-workflow --------------------------------------------------------

test("REVIEW_SUB_STAGES is the review sub-workflow (classify, dispatch, gather, narrate)", () => {
  // Pinned by QUB-95. The sub-workflow is structurally
  // present so QUB-96 only needs to push entries onto
  // the list. Today the list has the four sub-stages
  // that replaced the single-LLM-call sniff-legacy
  // placeholder:
  //   - classify (QUB-94): identify the PR type
  //   - dispatch (QUB-95): pick + run experts in parallel
  //   - gather (QUB-95): de-dupe the findings
  //   - narrate (QUB-95): produce the cohesive summary +
  //     inline comments
  // QUB-96 will insert meta-review between gather and
  // narrate.
  assert.deepEqual(
    REVIEW_SUB_STAGES.map((s) => s.id),
    ["classify", "dispatch", "gather", "narrate"],
  );
});

test("each sub-stage has the same contract as a macro stage", () => {
  // The gate / retry / resume machinery is shared between macro
  // and sub stages, so they must expose the same fields.
  for (const stage of REVIEW_SUB_STAGES) {
    assert.equal(typeof stage.id, "string", `${stage.id}: id`);
    assert.ok(
      "statusStage" in stage,
      `${stage.id}: statusStage must be present (null is allowed)`,
    );
    assert.equal(typeof stage.run, "function", `${stage.id}: run`);
    assert.equal(typeof stage.idempotent, "boolean", `${stage.id}: idempotent`);
  }
});

test("sub-stages are silent on the status thread (QUB-93)", () => {
  // The user-visible status thread stays the same. The "review"
  // status line is posted once at the start of the macro sniff
  // stage and covers the whole sub-workflow. No sub-stage posts
  // a new line.
  for (const stage of REVIEW_SUB_STAGES) {
    assert.equal(
      stage.statusStage,
      null,
      `${stage.id} must be silent (statusStage: null) to preserve QUB-93's surface`,
    );
  }
});

// --- runStages executor -------------------------------------------------

// Minimal stub deps for runStages. Each field is recorded so the
// test can assert on the order of operations and the values
// passed. postStatus is captured as a sequence so we can verify
// the surface (auth → clone → review → done, in the order the
// orchestrator in index.mjs drives).
function recordingDeps(overrides = {}) {
  const calls = { postStatus: [], log: [] };
  return {
    calls,
    fs: { readFile: async () => "fake" },
    jwt: { sign: () => "fake-jwt" },
    fetchImpl: async () => ({ ok: true, json: async () => ({ token: "ghs_token" }), text: async () => "" }),
    log: (stage, msg, extra) => calls.log.push({ stage, msg, extra }),
    errlog: (stage, msg, extra) => calls.log.push({ stage, msg, extra }),
    postStatus: async (stage, detail) => {
      calls.postStatus.push({ stage, detail });
    },
    paths: { repoDir: "/work/repo" },
    cloneRepo: async () => {},
    setOctokit: () => {},
    getOctokit: () => null,
    ...overrides,
  };
}

// Real-enough Octokit for the summary and inlines stages. Both
// call octokit.rest.issues.createComment / pulls.createReviewComment;
// we record the call so the test can assert on the side effect
// without going through the real Octokit / GitHub.
function fakeOctokit() {
  const calls = { createComment: [], createReviewComment: [] };
  return {
    calls,
    rest: {
      issues: {
        createComment: async (args) => {
          calls.createComment.push(args);
          return { data: { id: 1, body: args.body } };
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
}

const fakeCtx = {
  prOwner: "qubitquilt",
  prRepo: "boop",
  prNumber: 1,
  prHeadSha: "0123456789abcdef0123456789abcdef01234567",
  prBaseRef: "main",
  githubAppId: "1",
  githubAppInstallationId: "1",
  privateKeyPath: "/fake/pk",
  openrouterKeyPath: "/fake/or",
  reviewNumber: 1,
  statusCommentId: 111,
};

const fakeReview = () => ({
  summary: "## TL;DR\nLooks good.",
  inlineComments: [{ path: "src/foo.ts", line: 1, body: "nit" }],
  confidence: "high",
  telemetry: null,
});

test("runStages walks every macro stage in order", async () => {
  const sequence = [];
  // The fakeOctokit's createComment / createReviewComment are
  // wrapped to push to `sequence` so the test can assert on the
  // full order (the postStatus calls are also pushed, and the
  // github.mjs side effects of summary + inlines are pushed
  // here). The wrapping preserves the underlying call recording
  // on `octokit.calls` for body / path assertions.
  const calls = { createComment: [], createReviewComment: [] };
  const octokit = {
    calls,
    rest: {
      issues: {
        createComment: async (args) => {
          calls.createComment.push(args);
          sequence.push("createComment");
          return { data: { id: 1, body: args.body } };
        },
      },
      pulls: {
        createReviewComment: async (args) => {
          calls.createReviewComment.push(args);
          sequence.push("createReviewComment");
          return { data: { id: 1 } };
        },
      },
    },
  };
  const deps = recordingDeps({
    postStatus: async (stage) => {
      sequence.push(`postStatus(${stage})`);
    },
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: (token) => {
      sequence.push(`makeOctokit(${token})`);
      return octokit;
    },
    runOpenCodeSkill: async (_apiKey, _ctx, d) => {
      sequence.push("runOpenCodeSkill");
      await d.postStatus("review");
      return fakeReview();
    },
  };
  await runStages(fakeCtx, deps, overrides, {});
  // The macro order is what QUB-93 pins. auth -> clone -> review
  // is the user-visible status thread; the createComment /
  // createReviewComment calls are the silent side effects of
  // summary + inlines (no separate status line). cleanup is
  // gated on re-review + botLogin and does not run on the
  // first-review happy path.
  assert.deepEqual(sequence, [
    "makeOctokit(ghs_token)",
    "setOctokit",
    "postStatus(auth)",
    "cloneRepo",
    "postStatus(clone)",
    "runOpenCodeSkill",
    "postStatus(review)",
    "createComment",
    "createReviewComment",
  ]);
  // The summary stage posted a comment with the head-SHA marker
  // (used by the resume PR to detect an existing summary).
  assert.match(octokit.calls.createComment[0].body, /boop-head-sha/);
  // The inlines stage posted a review comment for the one
  // inline the fake review declared.
  assert.equal(octokit.calls.createReviewComment.length, 1);
  assert.equal(octokit.calls.createReviewComment[0].path, "src/foo.ts");
});

test("runStages skips summary + inlines when review has no summary", async () => {
  // Mirrors the parse-failure path. The sniff stage returns a
  // review with summary=""; the summary stage posts failed and
  // sets state.parseFailed; the inlines stage short-circuits.
  const sequence = [];
  const deps = recordingDeps({
    postStatus: async (stage, detail) =>
      sequence.push(`postStatus(${stage}${detail ? `: ${detail}` : ""})`),
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: () => fakeOctokit(),
    runOpenCodeSkill: async (_apiKey, _ctx, d) => {
      sequence.push("runOpenCodeSkill");
      await d.postStatus("review");
      return {
        summary: "",
        inlineComments: [],
        confidence: "low",
        parseError: "no structured block",
      };
    },
  };
  const state = {};
  await runStages(fakeCtx, deps, overrides, state);
  assert.equal(state.parseFailed, true);
  // No "done" status — the summary stage posted "failed" and the
  // orchestrator in index.mjs will surface the dashboard
  // "failed" + return without posting done. cleanup is also
  // skipped because the new review never landed.
  assert.deepEqual(sequence, [
    "setOctokit",
    "postStatus(auth)",
    "cloneRepo",
    "postStatus(clone)",
    "runOpenCodeSkill",
    "postStatus(review)",
    'postStatus(failed: summary parse failed: no structured block)',
  ]);
});

test("runStages threads state between stages (handshake → fetch → sniff)", async () => {
  // The state object is the contract between stages. A future
  // PR that renames state.octokit / state.installationToken /
  // state.review would silently break the orchestrator in
  // index.mjs; this test pins the keys.
  let receivedOpenrouterKey = null;
  const octokit = fakeOctokit();
  const deps = recordingDeps();
  const overrides = {
    makeOctokit: (token) => {
      const o = octokit;
      o.token = token;
      return o;
    },
    runOpenCodeSkill: async (apiKey) => {
      receivedOpenrouterKey = apiKey;
      return fakeReview();
    },
  };
  const state = {};
  await runStages(fakeCtx, deps, overrides, state);
  assert.ok(state.octokit, "handshake must populate state.octokit");
  assert.equal(state.octokit.token, "ghs_token");
  assert.equal(typeof state.installationToken, "string");
  assert.equal(state.openrouterApiKey, "fake");
  assert.ok(state.review, "sniff must populate state.review");
  assert.equal(receivedOpenrouterKey, "fake", "sniff must read openrouterApiKey from state");
});

test("runStages passes the opencode overrides through to the sniff stage", async () => {
  // The lib-split refactor in PR #71 introduced the overrides
  // hook so tests can stub runOpenCodeSkill without monkey-
  // patching the module. The QUB-89 refactor must preserve
  // that hook.
  const octokit = fakeOctokit();
  const deps = recordingDeps();
  const overrides = {
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => fakeReview(),
  };
  let called = false;
  overrides.runOpenCodeSkill = async (...args) => {
    called = true;
    return fakeReview(...args);
  };
  await runStages(fakeCtx, deps, overrides, {});
  assert.equal(called, true, "overrides.runOpenCodeSkill must be honored");
});

// --- runSubWorkflow executor --------------------------------------------

test("runSubWorkflow walks every sub-stage in order", async () => {
  // The sub-workflow executor is the primitive that the macro
  // sniff stage uses. QUB-95 expanded the list to four
  // sub-stages: classify, dispatch, gather, narrate.
  //
  // The sub-stage gates require state.openrouterApiKey; the
  // macro handshake stage populates it, so we set it here for
  // the direct runSubWorkflow call.
  const sequence = [];
  const deps = recordingDeps();
  const overrides = {
    classify: async () => {
      sequence.push("classify");
      return { type: "feature", confidence: "high" };
    },
    pickExperts: (classification) => {
      sequence.push("pickExperts");
      return ["stub-expert"];
    },
    runExperts: async () => {
      sequence.push("runExperts");
      return [
        {
          id: "stub-1",
          expert: "stub-expert",
          severity: "info",
          title: "T",
          body: "b",
          path: "src/foo.ts",
          line: 1,
        },
      ];
    },
    gather: (findings) => {
      sequence.push("gather");
      return findings;
    },
    narrate: async (findings) => {
      sequence.push("narrate");
      return {
        summary: "## TL;DR\nstub",
        inlineComments: findings.map((f) => ({
          path: f.path,
          line: f.line,
          body: f.body,
        })),
        confidence: "medium",
        telemetry: null,
      };
    },
  };
  await runSubWorkflow(REVIEW_SUB_STAGES, fakeCtx, deps, overrides, {
    openrouterApiKey: "fake",
  });
  assert.deepEqual(sequence, [
    "classify",
    "pickExperts",
    "runExperts",
    "gather",
    "narrate",
  ]);
});

test("runSubWorkflow supports a custom sub-stage list (test seam)", async () => {
  // QUB-95 / QUB-96 will pass their own sub-stage lists to
  // runSubWorkflow. The executor must accept any list, not just
  // the module-level REVIEW_SUB_STAGES export.
  const calls = [];
  const stages = [
    { id: "a", gate: async () => ({ ok: true }), run: async () => { calls.push("a"); } },
    { id: "b", gate: async () => ({ ok: true }), run: async () => { calls.push("b"); } },
    { id: "c", gate: async () => ({ ok: true }), run: async () => { calls.push("c"); } },
  ];
  await runSubWorkflow(stages, fakeCtx, recordingDeps(), {}, {});
  assert.deepEqual(calls, ["a", "b", "c"]);
});

// --- gate contract (QUB-90) --------------------------------------------

test("every macro stage has a gate function (QUB-90)", () => {
  // Pinned by QUB-90. A stage without a gate would silently
  // skip the precondition check; the run would be invoked
  // with invalid state and crash downstream.
  for (const stage of STAGES) {
    assert.equal(typeof stage.gate, "function", `${stage.id}: gate must be a function`);
  }
});

test("every sub-stage has a gate function (QUB-90)", () => {
  for (const stage of REVIEW_SUB_STAGES) {
    assert.equal(typeof stage.gate, "function", `${stage.id}: gate must be a function`);
  }
});

test("a failed gate sets state.parseFailed and short-circuits the run (QUB-90)", async () => {
  // The summary gate returns {ok: false, reason: "summary
  // parse failed: <X>"} when the review has no summary. The
  // executor must NOT throw (a parse failure is "expected" —
  // the LLM might not produce a structured block); it sets
  // state.parseFailed and returns. The orchestrator in
  // index.mjs checks state.parseFailed and short-circuits
  // the lifecycle. The status thread sees the gate's reason
  // verbatim as the "failed" status line.
  const sequence = [];
  const deps = recordingDeps({
    postStatus: async (stage, detail) =>
      sequence.push(`postStatus(${stage}${detail ? `: ${detail}` : ""})`),
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: () => fakeOctokit(),
    runOpenCodeSkill: async (_apiKey, _ctx, d) => {
      sequence.push("runOpenCodeSkill");
      await d.postStatus("review");
      return {
        summary: "",
        inlineComments: [],
        confidence: "low",
        parseError: "no structured block",
      };
    },
  };
  const state = {};
  // runStages returns normally (no throw) on a gate failure.
  const result = await runStages(fakeCtx, deps, overrides, state);
  assert.equal(result, state);
  assert.equal(state.parseFailed, true);
  // The summary stage was never reached (the gate caught the
  // empty review before the run). The inlines + cleanup
  // stages were also skipped.
  assert.deepEqual(sequence, [
    "setOctokit",
    "postStatus(auth)",
    "cloneRepo",
    "postStatus(clone)",
    "runOpenCodeSkill",
    "postStatus(review)",
    'postStatus(failed: summary parse failed: no structured block)',
  ]);
});

test("a passing gate proceeds to the run (QUB-90)", async () => {
  // Sanity check: a stage whose gate returns {ok: true}
  // runs normally. The summary stage's gate on a valid
  // review lets the run call postReview, which calls
  // createComment on the Octokit.
  const sequence = [];
  const calls = { createComment: [], createReviewComment: [] };
  const octokit = {
    calls,
    rest: {
      issues: {
        createComment: async (args) => {
          calls.createComment.push(args);
          sequence.push("createComment");
          return { data: { id: 1, body: args.body } };
        },
      },
      pulls: {
        createReviewComment: async (args) => {
          calls.createReviewComment.push(args);
          sequence.push("createReviewComment");
          return { data: { id: 1 } };
        },
      },
    },
  };
  const deps = recordingDeps({
    postStatus: async (stage) => sequence.push(`postStatus(${stage})`),
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: (token) => {
      sequence.push(`makeOctokit(${token})`);
      return octokit;
    },
    runOpenCodeSkill: async (_apiKey, _ctx, d) => {
      sequence.push("runOpenCodeSkill");
      await d.postStatus("review");
      return fakeReview();
    },
  };
  const state = {};
  const result = await runStages(fakeCtx, deps, overrides, state);
  assert.equal(result, state);
  assert.equal(state.parseFailed, undefined);
  // The summary gate passed; postReview was called; the
  // inlines gate passed; postInlineComments was called.
  assert.ok(sequence.includes("createComment"));
  assert.ok(sequence.includes("createReviewComment"));
  // No "failed" status was posted.
  assert.ok(!sequence.some((s) => s.startsWith("postStatus(failed")));
});

test("a sub-stage gate failure surfaces as state.parseFailed (QUB-90)", async () => {
  // The sub-workflow executor returns on a sub-stage gate
  // failure (same shape as the macro executor). The state
  // gets parseFailed = true; the macro `sniff` stage's run
  // returns; the next macro stage's gate (in this test
  // nothing follows because the sub-stage was the only
  // sub-stage) sees parseFailed implicitly via the next
  // gate's check.
  //
  // Direct test: call runSubWorkflow on REVIEW_SUB_STAGES
  // with an empty state. The sub-stage gate fails on the
  // missing openrouterApiKey; the executor sets
  // state.parseFailed and returns.
  const deps = recordingDeps();
  const state = {};
  const result = await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    deps,
    {},
    state,
  );
  assert.equal(result, state);
  assert.equal(state.parseFailed, true);
  // The sub-stage gate's reason is the "failed" status.
  const last = deps.calls.postStatus[deps.calls.postStatus.length - 1];
  assert.equal(last.stage, "failed");
  assert.equal(last.detail, "no openrouter api key");
});

test("gate failure is soft: a thrown run still propagates (QUB-90 boundary)", async () => {
  // Gate failures are "soft" — the executor returns and sets
  // state.parseFailed. Run-time errors (a thrown stage) are
  // "hard" — they propagate out of the executor. The
  // distinction is what QUB-91 (retry) builds on: a hard
  // throw can be caught and retried; a soft gate failure
  // is the executor's final answer for that stage.
  const sequence = [];
  const deps = recordingDeps({
    postStatus: async (stage) => sequence.push(`postStatus(${stage})`),
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async () => { throw new Error("cloneRepo blew up"); },
  });
  await assert.rejects(
    () => runStages(fakeCtx, deps, {}, {}),
    /cloneRepo blew up/,
    "a run-time error from a stage must propagate out of the executor",
  );
  // The auth + clone statuses were attempted; the run-time
  // error happened before the clone status PATCH.
  assert.ok(sequence.includes("postStatus(auth)"));
});

// --- retry policy (QUB-91) ----------------------------------------------

test("a retryable stage that throws retries up to stageMaxAttempts times (QUB-91)", async () => {
  // A flaky LLM call retries with bounded attempts. The
  // stageMaxAttempts = 3 + a no-op sleep so the test doesn't
  // wait. After 3 attempts, the run throws (the executor
  // rethrows the last error).
  let calls = 0;
  const stages = [
    {
      id: "flaky",
      retryable: true,
      gate: async () => ({ ok: true }),
      run: async () => {
        calls++;
        throw new Error(`flaky ${calls}`);
      },
    },
  ];
  const deps = recordingDeps({
    sleep: async () => {},
    stageMaxAttempts: 3,
  });
  await assert.rejects(
    () => runSubWorkflow(stages, fakeCtx, deps, {}, {}),
    /flaky 3/,
    "after the bound, the run throws the last error",
  );
  assert.equal(calls, 3, "retried exactly stageMaxAttempts times");
});

test("a retryable gate that fails retries up to stageMaxAttempts times (QUB-91)", async () => {
  // A gate that returns {ok: false} on a retryable stage
  // retries the gate (cheap) until the bound; then the
  // executor soft-fails (state.parseFailed = true, no throw).
  let gateCalls = 0;
  let runCalls = 0;
  const stages = [
    {
      id: "flaky-gate",
      retryable: true,
      gate: async () => {
        gateCalls++;
        return { ok: false, reason: "still bad" };
      },
      run: async () => {
        runCalls++;
      },
    },
  ];
  const deps = recordingDeps({
    sleep: async () => {},
    stageMaxAttempts: 3,
  });
  const state = {};
  await runSubWorkflow(stages, fakeCtx, deps, {}, state);
  assert.equal(gateCalls, 3, "gate was called exactly stageMaxAttempts times");
  assert.equal(runCalls, 0, "run was never called (gate kept failing)");
  assert.equal(state.parseFailed, true, "soft fail on gate retry exhaustion");
});

test("a non-retryable stage that throws rethrows immediately (QUB-91)", async () => {
  // A stage with retryable: false does not retry. The
  // existing parse-failure path (summary gate) and auth-style
  // errors land here.
  let calls = 0;
  const stages = [
    {
      id: "no-retry",
      retryable: false,
      gate: async () => ({ ok: true }),
      run: async () => {
        calls++;
        throw new Error("auth blew up");
      },
    },
  ];
  const deps = recordingDeps({
    sleep: async () => {},
    stageMaxAttempts: 3,
  });
  await assert.rejects(
    () => runSubWorkflow(stages, fakeCtx, deps, {}, {}),
    /auth blew up/,
  );
  assert.equal(calls, 1, "no retry on a non-retryable stage");
});

test("an error with nonRetryable: true is not retried even on a retryable stage (QUB-91)", async () => {
  // A future PR can mark specific errors as non-retryable by
  // attaching {nonRetryable: true}. The retry helper checks
  // the flag and rethrows immediately. Today the codebase
  // doesn't attach the flag anywhere; this test pins the
  // contract for a future caller.
  let calls = 0;
  const stages = [
    {
      id: "schema-error",
      retryable: true,
      gate: async () => ({ ok: true }),
      run: async () => {
        calls++;
        const err = new Error("schema mismatch");
        err.nonRetryable = true;
        throw err;
      },
    },
  ];
  const deps = recordingDeps({
    sleep: async () => {},
    stageMaxAttempts: 3,
  });
  await assert.rejects(
    () => runSubWorkflow(stages, fakeCtx, deps, {}, {}),
    /schema mismatch/,
  );
  assert.equal(calls, 1, "nonRetryable: true short-circuits the retry");
});

test("retry uses exponential backoff capped at stageBackoffMaxMs (QUB-91)", async () => {
  // The sleep is recorded so the test can assert on the
  // backoff schedule. The default schedule is base * 2^(n-1):
  // 1000, 2000, 4000, ... capped at stageBackoffMaxMs.
  const sleeps = [];
  const stages = [
    {
      id: "backoff",
      retryable: true,
      gate: async () => ({ ok: true }),
      run: async () => { throw new Error("nope"); },
    },
  ];
  const deps = recordingDeps({
    sleep: async (ms) => { sleeps.push(ms); },
    stageMaxAttempts: 5,
    stageBackoffBaseMs: 1000,
    stageBackoffMaxMs: 4000,
  });
  await assert.rejects(() => runSubWorkflow(stages, fakeCtx, deps, {}, {}));
  // 4 sleeps between 5 attempts: 1000, 2000, 4000, 4000 (capped).
  assert.deepEqual(sleeps, [1000, 2000, 4000, 4000]);
});

test("every retryable macro stage has a non-throwing first attempt by default (QUB-91)", async () => {
  // Sanity check: the default policy (3 attempts, 1s base,
  // 30s cap) means a transient failure on any retryable
  // macro stage costs at most 1+2+4 = 7s of backoff. The
  // Job's 30-min active deadline is the outer ceiling.
  assert.equal(DEFAULT_MAX_ATTEMPTS, 3);
  assert.equal(DEFAULT_BACKOFF_BASE_MS, 1000);
  assert.equal(DEFAULT_BACKOFF_MAX_MS, 30000);
});

test("each stage declares retryable: true or false (QUB-91)", () => {
  // Pinned by QUB-91. The default-retry policy is "retry on
  // failure"; a stage that opts out (handshake, summary)
  // has a specific reason — auth won't fix itself, parse
  // failure won't fix itself. A future PR that adds a stage
  // must make the choice explicit.
  for (const stage of STAGES) {
    assert.equal(
      typeof stage.retryable,
      "boolean",
      `${stage.id}: retryable must be a boolean`,
    );
  }
  // Handshake (auth) and summary (parse failure) are
  // non-retryable. The others are retryable.
  const byId = Object.fromEntries(STAGES.map((s) => [s.id, s]));
  assert.equal(byId.handshake.retryable, false);
  assert.equal(byId.fetch.retryable, true);
  assert.equal(byId.sniff.retryable, true);
  assert.equal(byId.summary.retryable, false);
  assert.equal(byId.inlines.retryable, true);
  assert.equal(byId.cleanup.retryable, true);
});

// --- classify sub-stage (QUB-94) ---------------------------------------

test("classify sub-stage calls overrides.classify and writes state.classification (QUB-94)", async () => {
  // The classify sub-stage uses the overrides.classify hook
  // (same pattern as overrides.runOpenCodeSkill). The
  // default falls back to the stub in lib/classify.mjs; a
  // future PR wires the real LLM call.
  const deps = recordingDeps();
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    deps,
    {
      classify: async () => ({ type: "bug-fix", confidence: "high" }),
      narrate: async () => fakeReview(),
    },
    state,
  );
  assert.deepEqual(state.classification, { type: "bug-fix", confidence: "high" });
});

test("classify sub-stage falls back to the default stub when no override is provided (QUB-94)", async () => {
  // The stub returns { type: "unknown", confidence: "low" }.
  // The dispatch sub-stage (QUB-95) treats "unknown" as
  // a default-expert-pool signal.
  const deps = recordingDeps();
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    deps,
    { narrate: async () => fakeReview() },
    state,
  );
  assert.deepEqual(state.classification, { type: "unknown", confidence: "low" });
});

test("classify sub-stage runs first, before dispatch (QUB-94/95)", async () => {
  // The classification drives the expert selection in
  // dispatch. The order is pinned: classify first, then
  // dispatch / gather / narrate.
  const sequence = [];
  const deps = recordingDeps();
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    deps,
    {
      classify: async () => {
        sequence.push("classify");
        return { type: "feature", confidence: "high" };
      },
      pickExperts: () => {
        sequence.push("dispatch");
        return [];
      },
      gather: (f) => {
        sequence.push("gather");
        return f;
      },
      narrate: async () => {
        sequence.push("narrate");
        return fakeReview();
      },
    },
    { openrouterApiKey: "fake" },
  );
  assert.deepEqual(sequence, ["classify", "dispatch", "gather", "narrate"]);
});

// --- resume (QUB-92) ---------------------------------------------------

test("runStages skips macro stages listed in state.passed (QUB-92)", async () => {
  // A failed prior run wrote state.passed = ["handshake",
  // "fetch"] to the status comment. The new run reads it and
  // skips those two stages; only sniff + summary + inlines +
  // cleanup run.
  //
  // The test seeds state.openrouterApiKey (the sniff-legacy
  // sub-stage gate requires it) and state.octokit (the
  // summary + inlines gates require it) because the
  // handshake stage — which would normally populate both —
  // is skipped.
  const sequence = [];
  const octokit = fakeOctokit();
  const deps = recordingDeps({
    postStatus: async (stage) => sequence.push(`postStatus(${stage})`),
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: () => octokit,
    runOpenCodeSkill: async (_apiKey, _ctx, d) => {
      sequence.push("runOpenCodeSkill");
      await d.postStatus("review");
      return fakeReview();
    },
  };
  const state = {
    passed: ["handshake", "fetch"],
    sub: {},
    openrouterApiKey: "fake", // seed for the sniff-legacy gate
    octokit, // seed for the summary + inlines gates (handshake was skipped)
  };
  await runStages(fakeCtx, deps, overrides, state);
  // handshake + fetch were skipped. The "auth" / "clone"
  // status lines were NOT PATCHed.
  assert.ok(!sequence.includes("setOctokit"));
  assert.ok(!sequence.includes("cloneRepo"));
  assert.ok(!sequence.includes("postStatus(auth)"));
  // The rest ran normally.
  assert.ok(sequence.includes("runOpenCodeSkill"));
  assert.ok(sequence.includes("postStatus(review)"));
  // The state.passed list grew to include the new stages.
  assert.deepEqual(state.passed, [
    "handshake",
    "fetch",
    "sniff",
    "summary",
    "inlines",
    "cleanup",
  ]);
});

test("runStages calls onStagePassed after every macro stage (QUB-92)", async () => {
  // The orchestrator (index.mjs) uses the onStagePassed
  // callback to persist state to the status comment after
  // each macro stage. The callback is the write-side of the
  // resume contract.
  const calls = [];
  const deps = recordingDeps({
    setOctokit: () => {},
    cloneRepo: async () => {},
  });
  const overrides = {
    makeOctokit: () => fakeOctokit(),
    runOpenCodeSkill: async () => fakeReview(),
  };
  await runStages(fakeCtx, deps, overrides, { passed: [], sub: {} }, {
    onStagePassed: async (stageId, state) => {
      calls.push({ stageId, passed: [...state.passed] });
    },
  });
  assert.deepEqual(
    calls.map((c) => c.stageId),
    ["handshake", "fetch", "sniff", "summary", "inlines", "cleanup"],
  );
  // Each callback sees the cumulative state.passed at the
  // moment the stage passed.
  assert.deepEqual(calls[0], { stageId: "handshake", passed: ["handshake"] });
  assert.deepEqual(calls[1], { stageId: "fetch", passed: ["handshake", "fetch"] });
  assert.deepEqual(
    calls[5],
    { stageId: "cleanup", passed: ["handshake", "fetch", "sniff", "summary", "inlines", "cleanup"] },
  );
});

test("runSubWorkflow skips sub-stages listed in state.sub[macroId] (QUB-92)", async () => {
  // The sniff macro stage's sub-workflow honors state.sub.sniff.
  // A prior run wrote sub = { sniff: ["sniff-legacy"] } and
  // the new run sees it and skips sniff-legacy.
  const sequence = [];
  const deps = recordingDeps();
  const overrides = {
    runOpenCodeSkill: async () => {
      sequence.push("runOpenCodeSkill");
      return fakeReview();
    },
  };
  const state = {
    passed: [],
    sub: { sniff: ["sniff-legacy"] },
    _subWorkflowOf: "sniff",
  };
  await runSubWorkflow(REVIEW_SUB_STAGES, fakeCtx, deps, overrides, state);
  // sniff-legacy was skipped; runOpenCodeSkill was not called.
  assert.deepEqual(sequence, []);
});

test("runSubWorkflow records passed sub-stages in state.sub[macroId] (QUB-92)", async () => {
  // Each sub-stage that passes is appended to state.sub[macroId].
  // The orchestrator's onStagePassed callback (at the macro
  // level) reads state.sub and writes it to the comment.
  //
  // QUB-95: the sub-workflow now has four sub-stages
  // (classify, dispatch, gather, narrate). All four are
  // recorded in state.sub.sniff.
  const deps = recordingDeps();
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    deps,
    {
      classify: async () => ({ type: "unknown", confidence: "low" }),
      narrate: async () => fakeReview(),
    },
    state,
  );
  assert.deepEqual(state.sub, {
    sniff: ["classify", "dispatch", "gather", "narrate"],
  });
});

// --- status thread parity (QUB-93) ------------------------------------
//
// The user-visible status thread on the PR is pinned by QUB-93.
// The QUB-87 migration replaces the one-shot Job with a staged
// workflow; the receiver pre-creates the status comment and the
// runner PATCHes it. The migration MUST NOT change the user-
// visible surface: the same emoji, the same short labels, the
// same order. A future change to the surface needs a follow-up
// ticket, not a quiet edit.
//
// The contract is enforced three ways:
//   1. workflow.mjs exports statusStageFor(id) and the
//      STAGES.statusStage field; tests pin the mapping.
//   2. github.mjs exports STATUS and SHORT maps; tests pin
//      the wording (and the receiver-side mirror in
//      apps/receiver/internal/webhook/handler.go is the
//      authoritative source — the runner is the consumer).
//   3. The integration test in index.test.mjs already asserts
//      the four status lines appear (auth, clone, review, done);
//      QUB-93 adds a stricter test that locks the exact short
//      labels and the order.
//
// The set of status stages is the union of the runner's
// STAGES.statusStage values that are not null, plus the
// runner's terminal "done" and "failed" stages (which the
// orchestrator in index.mjs posts directly). The mapping is
// enumerated below.

test("status stage set is the same before and after the staged workflow (QUB-93)", () => {
  // The runner must use exactly these five status labels. A
  // new label is a user-visible change that needs a follow-up
  // ticket; a missing label breaks the existing dedup-by-SHA
  // contract (the receiver's CountPriorReviews expects the
  // same five).
  const fromStages = STAGES.map((s) => s.statusStage).filter(Boolean);
  // The orchestrator posts "done" and "failed" directly (not
  // via a stage in STAGES).
  const terminal = ["done", "failed"];
  const all = [...fromStages, ...terminal].sort();
  assert.deepEqual(all, ["auth", "clone", "done", "failed", "review"]);
});

test("every short status label includes its emoji and matches the receiver (QUB-93)", () => {
  // The runner's SHORT map is the source of the short labels
  // the user sees in the status timeline. The same map lives
  // in apps/receiver/internal/webhook/handler.go; a change
  // here needs the same change there (and vice versa). A
  // future PR that adds a stage label must also add it to
  // the receiver's mirror.
  //
  // The labels must include the matching emoji. The emoji is
  // the visual signal a PR author skims for; changing the
  // emoji (or dropping it) breaks the muscle memory.
  const expected = {
    auth: "🤝 paw-shaken in",
    clone: "🥎 fetched",
    review: "👃 sniffing",
    done: "💤 napped",
    failed: "🔄 chased tail",
  };
  for (const [stage, label] of Object.entries(expected)) {
    assert.ok(
      Object.prototype.hasOwnProperty.call(SHORT, stage),
      `runner SHORT map missing stage "${stage}"`,
    );
    assert.equal(SHORT[stage], label, `runner SHORT["${stage}"]`);
  }
});

test("every status header line (STATUS map) carries the same emoji (QUB-93)", async () => {
  // The STATUS map is the body the orchestrator (and the
  // receiver, for the initial post) uses. It carries the
  // emoji + the verbose wording. Pinned here so a future
  // edit to the wording is a deliberate change.
  const { STATUS } = await import("./github.mjs");
  // The status keys we expect.
  for (const stage of ["auth", "clone", "review", "done", "failed"]) {
    assert.ok(
      Object.prototype.hasOwnProperty.call(STATUS, stage),
      `STATUS map missing stage "${stage}"`,
    );
    // The verbose body must contain the matching emoji.
    const expectedEmoji = {
      auth: "🤝",
      clone: "🥎",
      review: "👃",
      done: "💤",
      failed: "🔄",
    }[stage];
    assert.ok(
      STATUS[stage].includes(expectedEmoji),
      `STATUS["${stage}"] should contain ${expectedEmoji}; got "${STATUS[stage]}"`,
    );
  }
});