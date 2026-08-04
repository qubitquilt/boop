import { test } from "node:test";
import assert from "node:assert/strict";

import {
  STAGES,
  REVIEW_SUB_STAGES,
  statusStageFor,
  runStages,
  runSubWorkflow,
} from "./workflow.mjs";

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

test("REVIEW_SUB_STAGES is the review sub-workflow (initially a single placeholder)", () => {
  // Pinned by QUB-89. The sub-workflow is structurally present
  // so QUB-94 / QUB-95 / QUB-96 only need to push entries onto
  // the list. Today there is exactly one sub-stage: sniff-legacy,
  // which calls the existing runOpenCodeSkill. The QUB-95 PR
  // expands the list to {classify, dispatch, gather, narrate} and
  // QUB-96 adds meta-review.
  assert.deepEqual(
    REVIEW_SUB_STAGES.map((s) => s.id),
    ["sniff-legacy"],
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
  // sniff stage uses. Today it walks a single placeholder; the
  // shape is in place so QUB-95 can push more entries.
  const sequence = [];
  const deps = recordingDeps();
  const overrides = {
    runOpenCodeSkill: async () => {
      sequence.push("runOpenCodeSkill");
      return fakeReview();
    },
  };
  await runSubWorkflow(REVIEW_SUB_STAGES, fakeCtx, deps, overrides, {});
  assert.deepEqual(sequence, ["runOpenCodeSkill"]);
});

test("runSubWorkflow supports a custom sub-stage list (test seam)", async () => {
  // QUB-95 / QUB-96 will pass their own sub-stage lists to
  // runSubWorkflow. The executor must accept any list, not just
  // the module-level REVIEW_SUB_STAGES export.
  const calls = [];
  const stages = [
    { id: "a", run: async () => { calls.push("a"); } },
    { id: "b", run: async () => { calls.push("b"); } },
    { id: "c", run: async () => { calls.push("c"); } },
  ];
  await runSubWorkflow(stages, fakeCtx, recordingDeps(), {}, {});
  assert.deepEqual(calls, ["a", "b", "c"]);
});
