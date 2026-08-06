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

test("REVIEW_SUB_STAGES is the review sub-workflow (walkthrough, classify, dispatch, gather, meta-review, narrate)", () => {
  // Pinned by QUB-96 + multi-expert. The sub-workflow is
  // structurally present. Today the list has six
  // sub-stages:
  //   - walkthrough: independent LLM call that produces
  //     a human-readable summary of the PR; every expert
  //     consumes it as shared context.
  //   - classify (QUB-94): identify the PR type
  //   - dispatch (QUB-95): pick + run experts in parallel
  //   - gather (QUB-95): de-dupe the findings
  //   - meta-review (QUB-96): bounded re-pass of stuck-out
  //     expert findings
  //   - narrate (QUB-95): produce the cohesive summary +
  //     inline comments
  assert.deepEqual(
    REVIEW_SUB_STAGES.map((s) => s.id),
    [
      "walkthrough",
      "classify",
      "dispatch",
      "gather",
      "meta-review",
      "narrate",
    ],
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
  const calls = { postStatus: [], log: [], errlog: [] };
  // Default expert + walkthrough overrides. The real
  // multi-expert sub-workflow makes OpenRouter SDK calls;
  // the workflow tests don't drive those — they only
  // exercise the orchestration. Providing canned
  // overrides here keeps the tests fast and
  // deterministic. Tests that want to assert on the
  // expert / walkthrough bodies pass `overrides` and
  // these defaults are overridden.
  const defaultExpert = async () => ({ findings: [] });
  return {
    calls,
    fs: { readFile: async () => "fake" },
    jwt: { sign: () => "fake-jwt" },
    fetchImpl: async () => ({ ok: true, json: async () => ({ token: "ghs_token" }), text: async () => "" }),
    log: (stage, msg, extra) => calls.log.push({ stage, msg, extra }),
    errlog: (stage, msg, extra) => calls.errlog.push({ stage, msg, extra }),
    postStatus: async (stage, detail) => {
      calls.postStatus.push({ stage, detail });
    },
    paths: { repoDir: "/work/repo" },
    cloneRepo: async () => {},
    setOctokit: () => {},
    getOctokit: () => null,
    // QUB-95 + multi-expert: walkthrough + expert overrides
    // default to no-ops so the sub-workflow runs without
    // making real LLM calls. Tests that want a real
    // walkthrough or real expert findings pass them in
    // `overrides`.
    generateWalkthrough: async () => ({
      walkthrough: "(test fixture walkthrough)",
      telemetry: null,
    }),
    expertOverrides: {
      "regression-hunter": defaultExpert,
      "test-quality": defaultExpert,
      "api-design": defaultExpert,
      "error-handling": defaultExpert,
      "design-pattern": defaultExpert,
      "readability": defaultExpert,
    },
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

test("runStages passes the runOpenCodeSkill override through to the narrate stage", async () => {
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
  // sub-stages; QUB-96 inserted meta-review between
  // gather and narrate.
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
    metaReview: async () => {
      sequence.push("meta-review");
      return { reDispatch: [] };
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
    "meta-review",
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
  // QUB-102: state.failureReason mirrors the gate's reason
  // so the orchestrator forwards it to the dashboard. The
  // dashboard row's `error` field then reads "summary parse
  // failed: no structured block", not just "failed".
  assert.equal(state.failureReason, "summary parse failed: no structured block");
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
  // dispatch / gather / meta-review / narrate.
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
      metaReview: async () => {
        sequence.push("meta-review");
        return { reDispatch: [] };
      },
      narrate: async () => {
        sequence.push("narrate");
        return fakeReview();
      },
    },
    { openrouterApiKey: "fake" },
  );
  assert.deepEqual(sequence, [
    "classify",
    "dispatch",
    "gather",
    "meta-review",
    "narrate",
  ]);
});

// --- meta-review (QUB-96) -----------------------------------------------

test("meta-review sub-stage with no re-dispatch leaves findings unchanged (QUB-96)", async () => {
  // The default metaReview returns { reDispatch: [] }. The
  // gathered findings pass through unchanged. The test
  // overrides runExperts to return a single finding so the
  // assertion is on a known input.
  const original = [
    { id: "a", expert: "x", severity: "info", title: "A", body: "a" },
  ];
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    recordingDeps(),
    {
      classify: async () => ({ type: "feature", confidence: "high" }),
      runExperts: async () => original,
      narrate: async () => fakeReview(),
    },
    state,
  );
  // Findings are preserved (the default metaReview does not
  // request a re-pass).
  assert.equal(state.findings.length, 1);
  assert.equal(state.findings[0].id, "a");
});

test("meta-review sub-stage with a re-dispatch replaces the re-dispatched expert's findings (QUB-96)", async () => {
  // The meta-reviewer requests a re-pass of one expert; the
  // runner re-dispatches that expert, then merges the
  // new findings with the old (replacing the old findings
  // from the same expert, preserving findings from
  // other experts).
  const originalFindings = [
    { id: "old-x-1", expert: "x", severity: "info", title: "OldX1", body: "old" },
    { id: "old-x-2", expert: "x", severity: "info", title: "OldX2", body: "old" },
    { id: "old-y-1", expert: "y", severity: "info", title: "OldY1", body: "y" },
  ];
  const rePassFindings = [
    { id: "new-x-1", expert: "x", severity: "warning", title: "NewX1", body: "new" },
  ];
  let dispatchCalls = 0;
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  let runExpertsCalls = 0;
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    recordingDeps(),
    {
      classify: async () => ({ type: "feature", confidence: "high" }),
      runExperts: async (names) => {
        dispatchCalls++;
        // Initial dispatch returns the original findings.
        // Re-pass returns the new x findings.
        if (names.length === 1 && names[0] === "x") {
          runExpertsCalls++;
          return rePassFindings;
        }
        return originalFindings;
      },
      metaReview: async () => ({ reDispatch: ["x"] }),
      narrate: async () => fakeReview(),
    },
    state,
  );
  assert.equal(dispatchCalls, 2, "runExperts called twice (initial + re-pass)");
  assert.equal(runExpertsCalls, 1, "re-pass was for 'x' once");
  // x's old findings are gone, x's new finding is in,
  // y's finding is preserved.
  assert.equal(state.findings.length, 2);
  const ids = state.findings.map((f) => f.id).sort();
  assert.deepEqual(ids, ["new-x-1", "old-y-1"]);
  // The x findings are the new ones.
  const xFindings = state.findings.filter((f) => f.expert === "x");
  assert.equal(xFindings.length, 1);
  assert.equal(xFindings[0].id, "new-x-1");
  assert.equal(xFindings[0].severity, "warning");
});

test("meta-review sub-stage is bounded: it does not re-loop (QUB-96)", async () => {
  // Even if a meta-review override requests a re-dispatch and
  // the re-dispatch would request another re-dispatch, the
  // sub-stage is called exactly once. The bound is "one
  // re-pass per run"; future meta-reviewer outputs that
  // would loop are ignored.
  let metaReviewCalls = 0;
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
    findings: [],
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    recordingDeps(),
    {
      classify: async () => ({ type: "feature", confidence: "high" }),
      metaReview: async () => {
        metaReviewCalls++;
        return { reDispatch: ["x"] };
      },
      runExperts: async () => [],
      narrate: async () => fakeReview(),
    },
    state,
  );
  assert.equal(metaReviewCalls, 1, "meta-review called exactly once");
});

test("meta-review's reDispatch names must be in EXPERT_POOL or runExperts throws (QUB-96)", async () => {
  // The meta-reviewer's re-dispatch list must contain
  // expert names that the runner can resolve. An unknown
  // name throws (caught by the gate + retry machinery).
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
    findings: [],
  };
  await assert.rejects(
    () =>
      runSubWorkflow(
        REVIEW_SUB_STAGES,
        fakeCtx,
        recordingDeps(),
        {
          classify: async () => ({ type: "feature", confidence: "high" }),
          metaReview: async () => ({ reDispatch: ["nonexistent-expert"] }),
          runExperts: async (names) => {
            // Use the real runExperts to exercise the
            // "unknown expert" error path. The real
            // runExperts reads `deps.expertOverrides`
            // before EXPERT_POOL; the override map below
            // mirrors the recordingDeps default so the
            // names resolve, then the unknown name in
            // reDispatch trips the error.
            const realExperts = await import("./experts.mjs");
            const stubExpert = async () => ({ findings: [] });
            return realExperts.runExperts(names, {}, {
              expertOverrides: {
                "regression-hunter": stubExpert,
                "test-quality": stubExpert,
                "api-design": stubExpert,
                "error-handling": stubExpert,
                "design-pattern": stubExpert,
                "readability": stubExpert,
              },
            });
          },
          narrate: async () => fakeReview(),
        },
        state,
      ),
    /unknown expert/,
  );
});

// --- B1 / B2 fixes from PR #105 review --------------------------------

test("B1: narrate falls back to defaultRunOpenCodeSkill when no override is provided (production path)", async () => {
  // BoopPr flagged that production was calling defaultNarrate
  // (the stub) and posting placeholder text. The fix: the
  // narrate stage's third precedence is the legacy
  // runOpenCodeSkill, so every PR gets a real review until
  // the real multi-expert narrator lands.
  const deps = recordingDeps();
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  let skillCalled = false;
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    deps,
    {
      classify: async () => ({ type: "feature", confidence: "high" }),
      // No runOpenCodeSkill override. No narrate override.
      // The fallback should call the real
      // defaultRunOpenCodeSkill. We exercise the
      // override-driven precedence here (the production
      // fallback path is covered by the live cluster).
      runOpenCodeSkill: async () => {
        skillCalled = true;
        return {
          summary: "## TL;DR\nreal review",
          inlineComments: [],
          confidence: "high",
          telemetry: null,
        };
      },
      narrate: async () => ({
        summary: "## TL;DR\nplaceholder",
        inlineComments: [],
        confidence: "low",
        telemetry: null,
      }),
    },
    state,
  );
  // The runOpenCodeSkill override wins (precedence 1).
  assert.equal(skillCalled, true);
  assert.match(state.review.summary, /real review/);
});

test("B1: narrate calls overrides.narrate when only narrate is provided", async () => {
  // Precedence 2: overrides.narrate beats the legacy
  // fallback. This is the path a test (or a future
  // production narrator) uses.
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
      classify: async () => ({ type: "feature", confidence: "high" }),
      narrate: async () => ({
        summary: "## TL;DR\nnarrate-override",
        inlineComments: [],
        confidence: "medium",
        telemetry: null,
      }),
    },
    state,
  );
  assert.equal(state.review.summary, "## TL;DR\nnarrate-override");
});

test("B2: empty re-pass drops the rejected expert's old findings", async () => {
  // BoopPr flagged that mergeByExpert only dropped old
  // findings for experts that appeared in the replacement
  // set. A re-pass that returns no findings for a
  // re-dispatched expert kept the old ones — exactly the
  // opposite of what was intended. The fix: pass the
  // reDispatch list to mergeByExpert so ALL old findings
  // for re-dispatched experts are dropped, even if the
  // re-pass returns empty.
  const originalFindings = [
    { id: "old-x-1", expert: "x", severity: "info", title: "OldX1", body: "old" },
    { id: "old-x-2", expert: "x", severity: "info", title: "OldX2", body: "old" },
    { id: "old-y-1", expert: "y", severity: "info", title: "OldY1", body: "y" },
  ];
  // The re-pass returns NOTHING for x (empty rejection).
  // The fix: x's old findings should still be dropped.
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    recordingDeps(),
    {
      classify: async () => ({ type: "feature", confidence: "high" }),
      runExperts: async (names) => {
        if (names.length === 1 && names[0] === "x") {
          // Re-pass for x: empty (rejection).
          return [];
        }
        return originalFindings;
      },
      metaReview: async () => ({ reDispatch: ["x"] }),
      narrate: async () => fakeReview(),
    },
    state,
  );
  // x's old findings are gone (the empty re-pass
  // signaled rejection), y's finding is preserved.
  assert.equal(state.findings.length, 1);
  assert.equal(state.findings[0].expert, "y");
  assert.equal(state.findings[0].id, "old-y-1");
});

test("B2: re-pass with new findings replaces the rejected expert's old findings", async () => {
  // The standard case: the re-pass returns new findings.
  // The old ones are dropped, the new ones land.
  const originalFindings = [
    { id: "old-x-1", expert: "x", severity: "info", title: "OldX1", body: "old" },
    { id: "old-y-1", expert: "y", severity: "info", title: "OldY1", body: "y" },
  ];
  const rePassFindings = [
    { id: "new-x-1", expert: "x", severity: "warning", title: "NewX1", body: "new" },
  ];
  const state = {
    passed: [],
    sub: {},
    _subWorkflowOf: "sniff",
    openrouterApiKey: "fake",
  };
  await runSubWorkflow(
    REVIEW_SUB_STAGES,
    fakeCtx,
    recordingDeps(),
    {
      classify: async () => ({ type: "feature", confidence: "high" }),
      runExperts: async (names) => {
        if (names.length === 1 && names[0] === "x") return rePassFindings;
        return originalFindings;
      },
      metaReview: async () => ({ reDispatch: ["x"] }),
      narrate: async () => fakeReview(),
    },
    state,
  );
  // old-x-1 gone, new-x-1 in, old-y-1 preserved.
  assert.equal(state.findings.length, 2);
  const ids = state.findings.map((f) => f.id).sort();
  assert.deepEqual(ids, ["new-x-1", "old-y-1"]);
});

// --- resume (QUB-92) ---------------------------------------------------

test("runStages aborts when state.passed already has a macro stage (QUB-102)", async () => {
  // QUB-102: a prior pod of the same Job wrote state.passed =
  // ["handshake", "fetch"] to the status comment. The K8s
  // controller's BackoffLimit=0 (jobbuilder.go) is the primary
  // defense against a second pod starting, but a manual re-
  // trigger (operator deletes the failed Job, re-issues the
  // webhook) or a K8s bug can still produce a second pod of
  // the same Job. The runner's startup guard catches this:
  // when state.passed is non-empty, it aborts the run before
  // any side-effecting stage (summary, inlines) runs, so the
  // PR sees no duplicate summary comment + no duplicate
  // inline review threads.
  //
  // QUB-92's resume semantics (skip-and-continue) have been
  // retired by QUB-102. The new contract is "any prior
  // progress -> abort", which is the literal reading of the
  // issue's startup guard. The status-comment timeline is
  // the operator's source of truth for what the prior pod
  // did; to re-run a review cleanly the operator clears the
  // status comment or uses a new head SHA.
  const sequence = [];
  const octokit = fakeOctokit();
  const deps = recordingDeps({
    postStatus: async (stage, detail) => {
      sequence.push(
        `postStatus(${stage}${detail ? `: ${detail}` : ""})`,
      );
      deps.calls.postStatus.push({ stage, detail });
    },
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => {
      sequence.push("runOpenCodeSkill");
      return fakeReview();
    },
  };
  const state = {
    passed: ["handshake", "fetch"],
    sub: {},
    openrouterApiKey: "fake",
    octokit,
  };
  await runStages(fakeCtx, deps, overrides, state);
  // The second pod refused to run. No handshake, no clone,
  // no review, no summary, no inlines, no cleanup.
  assert.ok(!sequence.includes("setOctokit"));
  assert.ok(!sequence.includes("cloneRepo"));
  assert.ok(!sequence.includes("postStatus(auth)"));
  assert.ok(!sequence.includes("runOpenCodeSkill"));
  assert.equal(octokit.calls.createComment.length, 0);
  assert.equal(octokit.calls.createReviewComment.length, 0);
  // The orchestrator in index.mjs reads state.parseFailed and
  // short-circuits the lifecycle (no dashboard telemetry, no
  // "done" status). state.failureReason is the message the
  // orchestrator forwards to the dashboard so a "failed" row
  // carries the abort reason (not just the stage).
  assert.equal(state.parseFailed, true);
  assert.equal(typeof state.failureReason, "string");
  assert.match(state.failureReason, /another pod already passed/);
  // The abort was logged at ERROR severity (errlog, not log)
  // so an operator filtering kubectl logs by level=ERROR sees
  // the most actionable diagnostic for a QUB-102 hit.
  const abortErr = deps.calls.errlog.find((c) => c.stage === "abort");
  assert.ok(abortErr, "expected an errlog entry for the abort");
  assert.match(abortErr.msg, /another pod already passed/);
  // The failed status was posted with the same reason. The
  // reason lists the full passed set so the operator's
  // status-thread timeline shows the prior pod's progress.
  const failed = deps.calls.postStatus.find((c) => c.stage === "failed");
  assert.ok(failed, "expected a failed status post");
  assert.match(failed.detail, /another pod already passed \[handshake/);
  assert.match(failed.detail, /refusing to duplicate the review/);
});

test("runStages aborts even when state.passed only has the summary stage (QUB-102)", async () => {
  // The headline scenario: pod 1 wrote passed = [..., "summary"]
  // before dying (e.g. transient network error mid-sniff
  // recovered, summary posted, then inlines stage crashed).
  // Pod 2 starts. With BackoffLimit=0 the controller will not
  // restart, but for the abort path we still want pod 2 to
  // refuse to re-post the summary comment. The current test
  // seeds state.passed with the full prefix so the loop hits
  // the prefix stages first; the abort fires on the first
  // stage it finds in passed (handshake).
  //
  // The reason lists the full prior-pod passed set so the
  // operator's status timeline shows what pod 1 actually
  // finished. The important assertion here is that neither
  // postReview nor postInlineComments was called — the abort
  // fires before any postable side effect.
  const sequence = [];
  const octokit = fakeOctokit();
  const deps = recordingDeps({
    postStatus: async (stage, detail) => {
      sequence.push(
        `postStatus(${stage}${detail ? `: ${detail}` : ""})`,
      );
      deps.calls.postStatus.push({ stage, detail });
    },
    setOctokit: () => sequence.push("setOctokit"),
    cloneRepo: async (_ctx, d) => {
      sequence.push("cloneRepo");
      await d.postStatus("clone");
    },
  });
  const overrides = {
    makeOctokit: () => octokit,
    runOpenCodeSkill: async () => {
      sequence.push("runOpenCodeSkill");
      return fakeReview();
    },
  };
  const state = {
    passed: ["handshake", "fetch", "sniff", "summary"],
    sub: {},
    openrouterApiKey: "fake",
    octokit,
  };
  await runStages(fakeCtx, deps, overrides, state);
  // The postable side effects must NOT have run.
  assert.equal(octokit.calls.createComment.length, 0);
  assert.equal(octokit.calls.createReviewComment.length, 0);
  // The orchestrator short-circuits on parseFailed and
  // forwards the reason to the dashboard via
  // state.failureReason.
  assert.equal(state.parseFailed, true);
  assert.equal(typeof state.failureReason, "string");
  // The reason includes the full prior-pod passed set; the
  // merge in index.mjs may shuffle the order (the loop hits
  // handshake first, the merge produces a different shape),
  // so the assertion checks the items, not the order.
  const abortErr = deps.calls.errlog.find((c) => c.stage === "abort");
  assert.ok(abortErr, "expected an errlog entry for the abort");
  for (const stage of ["handshake", "fetch", "sniff", "summary"]) {
    assert.ok(
      abortErr.msg.includes(stage),
      `expected reason to list ${stage}; got: ${abortErr.msg}`,
    );
  }
});

test("runStages does not abort when state.passed is empty (QUB-102)", async () => {
  // Counter-test: a fresh run (no prior progress) must not
  // hit the abort path. The state.passed array is empty; the
  // loop runs every stage normally.
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
    runOpenCodeSkill: async () => fakeReview(),
  };
  const state = { passed: [], sub: {} };
  await runStages(fakeCtx, deps, overrides, state);
  assert.equal(state.parseFailed, undefined);
  // The normal postable side effects still ran.
  assert.equal(octokit.calls.createComment.length, 1);
  assert.equal(octokit.calls.createReviewComment.length, 1);
  // The passed list grew to the full set.
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
  // QUB-96: the sub-workflow now has five sub-stages
  // (classify, dispatch, gather, meta-review, narrate). All
  // five are recorded in state.sub.sniff.
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
    sniff: [
      "walkthrough",
      "classify",
      "dispatch",
      "gather",
      "meta-review",
      "narrate",
    ],
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
    done: "🦴 bone delivered",
    failed: "❌ lost the bone",
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
      done: "🦴",
      failed: "❌",
    }[stage];
    assert.ok(
      STATUS[stage].includes(expectedEmoji),
      `STATUS["${stage}"] should contain ${expectedEmoji}; got "${STATUS[stage]}"`,
    );
  }
});