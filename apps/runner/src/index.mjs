// Boop runner.
//
// Orchestrates one PR review:
//   1. Reads the GitHub App private key and OpenRouter API key from
//      their mounted Secret files (not env — env is visible to a
//      prompt-injected LLM via /proc/self/environ).
//   2. Walks the staged workflow (handshake → fetch → sniff →
//      summary → inlines) defined in `./lib/workflow.mjs`.
//   3. PATCHes the status comment at each stage with the latest
//      emoji + message.
//   4. Posts the review body to the PR as a single comment, plus
//      the line-specific inline comments.
//
// Structure: this file is the orchestrator. The work is in
// `./lib/*.mjs` (config, log, security, git, opencode, github,
// dashboard, workflow). Each lib module accepts a `ctx` (loaded
// config) and a `deps` bundle so the whole pipeline is
// unit-testable without env vars, real Octokit, real network, or
// real `git`.

import fs from "node:fs/promises";
import jwt from "jsonwebtoken";
import { spawn } from "node:child_process";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import {
  loadConfig,
  REPO_DIR,
  CONFIG_SRC,
  WRITABLE_HOME,
  WRITABLE_CONFIG,
  CONFIG_DIR,
  NETRC_PATH,
  GITCONFIG_PATH,
} from "./lib/config.mjs";
import { makeLogger } from "./lib/log.mjs";
import { assertSafeRef, assertSafeSha } from "./lib/security.mjs";
import { createCleanupRegistry, cloneRepo } from "./lib/git.mjs";
import { runStages } from "./lib/workflow.mjs";
import {
  postStatus,
  ensureStatusComment,
  readWorkflowState,
  writeWorkflowState,
} from "./lib/github.mjs";
import { postStatus as postDashboardStatus, postTelemetry } from "./lib/dashboard.mjs";

const execFileAsync = promisify(execFile);

// makeDeps bundles every side-effecting dependency the pipeline
// passes around. Each lib function reads only what it needs from
// `deps`; tests can swap any field (spawnFn, fetchImpl, OctokitCtor)
// to drive a deterministic scenario.
//
// The octokit slot is shared between the handshake stage (which
// writes it after minting the installation token) and the
// `postStatus` closure (which reads it at PATCH time). The slot
// pattern lets the stage functions stay in workflow.mjs without
// reaching back into index.mjs to mutate module state.
//
// QUB-99: the status-comment slot mirrors the octokit slot.
// BOOP_STATUS_COMMENT_ID may arrive unset (the receiver no longer
// pre-creates the comment). On the first postStatus call the
// wrapper lazy-creates the initial status comment, caches the new
// id on the slot, and reuses it for every subsequent PATCH (so a
// pipeline retry hits the same comment instead of posting another
// one). The slot is mutable; pass-by-reference is the point.
//
// currentCtx returns the ctx to hand to anything that needs the
// live statusCommentId (postStatus PATCH, the QUB-92 resume
// read/write, the workflow-state handoff). Until the slot is
// populated by ensureStatusComment, the returned ctx has a null
// statusCommentId and the caller should skip — the same shape
// the env-var snapshot has on day one.
function makeDeps(ctx, log, cleanup) {
  const octokitSlot = { value: null };
  const statusCommentSlot = { value: ctx.statusCommentId || null };
  const getOctokit = () => octokitSlot.value;
  const currentCtx = () =>
    statusCommentSlot.value ? { ...ctx, statusCommentId: statusCommentSlot.value } : ctx;
  const patchStatusWithCtx = (stage, detail) =>
    postStatus(stage, detail, currentCtx(), {
      log: log.log,
      errlog: log.errlog,
      octokit: getOctokit(),
    });
  const postStatusWrapper = async (stage, detail) => {
    if (!getOctokit()) {
      log.log("status", "skip (no octokit yet)", { stage });
      return;
    }
    await ensureStatusComment(
      getOctokit(),
      ctx,
      { log: log.log, errlog: log.errlog },
      statusCommentSlot,
      ctx.triggeredBy || null,
    );
    await patchStatusWithCtx(stage, detail);
  };
  return {
    fs,
    execFile: execFileAsync,
    spawnFn: spawn,
    jwt,
    fetchImpl: fetch,
    OctokitCtor: undefined, // lib/github.mjs falls back to the real Octokit
    paths: {
      repoDir: REPO_DIR,
      configSrc: CONFIG_SRC,
      configDir: CONFIG_DIR,
      writableHome: WRITABLE_HOME,
      writableConfig: WRITABLE_CONFIG,
      netrc: NETRC_PATH,
      gitconfig: GITCONFIG_PATH,
    },
    cleanup,
    log: log.log,
    errlog: log.errlog,
    setOctokit: (octokit) => { octokitSlot.value = octokit; },
    getOctokit,
    currentCtx,
    postStatus: postStatusWrapper,
    // cloneRepo is wrapped so its postStatus call goes through the
    // same lazy octokit-resolution + status-comment-creation path
    // as the direct postStatus. The clone runs AFTER token minting
    // so the GitHub-App installation token is already populated.
    cloneRepo: (c, d) => cloneRepo(c, {
      ...d,
      postStatus: (s) => postStatusWrapper(s, undefined),
    }),
  };
}

// run is the pipeline. Exported so tests can drive it with a fixture
// ctx + injected deps; index.mjs is just the entry invocation.
//
// `overrides` lets a test swap any side-effecting dep (spawn, exec,
// fetch, jwt, fs) without monkey-patching the module — every lib
// function reads its dependencies off the `deps` bundle, so the
// override simply replaces the corresponding field. The individual
// stage functions in `workflow.mjs` also read `overrides.runOpenCodeSkill`
// and `overrides.makeOctokit` so the same override surface keeps
// working.
export async function run(env = process.env, overrides = {}) {
  const ctx = loadConfig(env);
  const log = makeLogger(ctx);
  const cleanup = createCleanupRegistry({ errlog: log.errlog });

  // The Octokit instance has its own copy of the token inside, but
  // the local var lives in this scope. There is no secure way to
  // wipe a JS string in-place (V8 interns and may share the backing
  // buffer), so we instead limit the token's lifetime to the rest
  // of this function and rely on the netrc + gitconfig cleanup
  // hooks (see writeNetrc / writeGitconfig in lib/git.mjs) to make
  // any persistent copy unreachable.
  const deps = {
    ...makeDeps(ctx, log, cleanup),
    ...overrides, // overrides.spawnFn, .execFile, .fetchImpl, .jwt, .fs
  };

  // Validate every PR-controlled refname BEFORE it touches `git` or
  // any subprocess argv. validateBaseRef in the receiver is the
  // first gate; this is the second (defense-in-depth: a future
  // change in the receiver shouldn't be load-bearing here).
  assertSafeRef("PR_BASE_REF", ctx.prBaseRef);
  if (ctx.previousHeadSha) {
    assertSafeSha("PR_PREVIOUS_HEAD_SHA", ctx.previousHeadSha);
  }

  log.log("start", "boop runner starting", {
    status_comment_id: ctx.statusCommentId,
    reaction_comment_id: ctx.reactionCommentId,
    review_number: ctx.reviewNumber,
    previous_head_sha: ctx.previousHeadSha,
    bot_login: ctx.botLogin,
  });

  // QUB-92 resume: state.passed is the list of macro stages
  // that have already landed (read from the status comment by
  // a prior run). The runner reads the state from the comment
  // after the handshake stage (we need the octokit first) and
  // writes it back after every macro stage that passes. A pod
  // kill mid-run preserves the last successful write.
  const state = { passed: [], sub: {} };

  // onStagePassed: invoked after every macro stage that
  // passes. The first invocation (handshake) reads the prior
  // workflow state from the comment and merges it. All
  // invocations write the updated state back. The two-phase
  // design means the comment is the source of truth: if a
  // prior run wrote "passed=[handshake,fetch]" and the pod
  // died before reaching sniff, this run will see that on
  // its handshake callback and skip the relevant stages.
  // QUB-92: stateInitialized is racy under concurrent
  // onStagePassed invocations. The macro stages in
  // runStages are strictly sequential today, so the flag
  // is set exactly once on the first stage's callback and
  // no later callback re-enters the read-merge-write block.
  // If a future change parallelises stages (QUB-91 already
  // added per-stage retries; a future PR could promote
  // some macro stages to a worker pool), this flag must
  // be guarded or replaced with a one-shot helper. The
  // read-merge-write block is NOT idempotent — running it
  // twice would re-read the same prior state and double-
  // filter, possibly dropping stages the current run
  // already passed.
  //
  // QUB-99 + QUB-92 follow-up: readWorkflowState and
  // writeWorkflowState both need the live statusCommentId
  // from the slot, not the env-var snapshot on ctx
  // (which is now null because the receiver no longer
  // pre-creates the comment). currentCtx() supplies the
  // effective ctx; the early-return on
  // !statusCommentSlot.value skips the read/write path
  // when the lazy-create has not happened yet (i.e., no
  // first postStatus call has run).
  let stateInitialized = false;
  const onStagePassed = async (stageId) => {
    const octokit = deps.getOctokit();
    if (!octokit) return;
    const liveCtx = deps.currentCtx();
    if (!liveCtx.statusCommentId) return;
    if (!stateInitialized) {
      const prior = await readWorkflowState(octokit, liveCtx, {
        log: log.log,
        errlog: log.errlog,
      });
      // Merge the prior passed list with the current run's
      // passed list. The current run started empty (state.passed
      // is [] above), so the merge is a union — the prior
      // run's passed stages are prepended.
      if (prior.passed && prior.passed.length > 0) {
        state.passed = [
          ...prior.passed.filter((id) => !state.passed.includes(id)),
          ...state.passed,
        ];
      }
      if (prior.sub && Object.keys(prior.sub).length > 0) {
        for (const [macroId, subs] of Object.entries(prior.sub)) {
          state.sub[macroId] = [
            ...(subs || []).filter((id) => !(state.sub[macroId] || []).includes(id)),
            ...(state.sub[macroId] || []),
          ];
        }
      }
      if (state.passed.length > 0 || Object.keys(state.sub).length > 0) {
        log.log("state", "resuming from prior run", {
          passed: state.passed,
          sub: state.sub,
        });
      }
      stateInitialized = true;
    }
    await writeWorkflowState(
      octokit,
      liveCtx,
      { log: log.log, errlog: log.errlog },
      { passed: state.passed, sub: state.sub },
    );
  };

  try {
    // The six macro stages: handshake → fetch → sniff → summary
    // → inlines → cleanup. Each stage reads from `state` (or
    // ctx) and writes its output to `state`. Throwing aborts the
    // run; the catch below translates the failure into a
    // status-comment PATCH + dashboard "failed". The cleanup
    // stage handles its own skip (first review / no botLogin) and
    // best-effort error swallow so the orchestrator stays
    // straightforward.
    await runStages(ctx, deps, overrides, state, {
      onStagePassed,
    });

    // Parse-failed sniff: the summary stage set state.parseFailed
    // and posted "failed" already. Skip the rest of the lifecycle
    // (no summary, no inlines, no cleanup of prior artifacts —
    // we did not post anything, so the prior review is still
    // current on the PR).
    //
    // QUB-102: state.failureReason was set by the abort path
    // (or by a soft gate failure in workflow.mjs). Forward it
    // to the dashboard so a "failed" row carries the reason;
    // otherwise the dashboard cannot distinguish a QUB-102
    // abort from a sniff-parse failure (both reach the
    // receiver as stage="failed").
    if (state.parseFailed) {
      await postDashboardStatus("failed", ctx, {
        log: log.log,
        fetchImpl: deps.fetchImpl,
        reason: state.failureReason,
      });
      return;
    }

    // Dashboard data layer: POST final telemetry (token usage
    // + cost) once the review is fully posted. Best-effort —
    // failures are logged inside postTelemetry.
    if (state.review && state.review.telemetry) {
      await postTelemetry(state.review.telemetry, ctx, { log: log.log, fetchImpl: deps.fetchImpl });
    }
    await postDashboardStatus("done", ctx, { log: log.log, fetchImpl: deps.fetchImpl });
    await deps.postStatus("done");
  } catch (err) {
    log.errlog("review", "staged workflow failed", { error: String(err?.message ?? err) });
    // Best-effort dashboard status so the dashboard's live
    // view shows the run as failed even if the post-pipeline
    // status PATCH (deps.postStatus) also fails. The reason
    // mirrors the GitHub-commented reason so the operator's
    // primary view matches the source of truth.
    const reason = state.failureReason || String(err?.message ?? err);
    await postDashboardStatus("failed", ctx, {
      log: log.log,
      fetchImpl: deps.fetchImpl,
      reason,
    });
    await deps.postStatus("failed", reason);
    throw err;
  } finally {
    // Always scrub credentials and tmp artefacts, even on failure.
    // Order matters: revoke the netrc / gitconfig before unlinking
    // them so a future `git fetch` against the in-memory state
    // cannot read the token.
    await cleanup.runAll();
  }
}

// Only invoke run() when this module is the process entry point.
// Tests import individual helpers via `import { run } from
// "./index.mjs"` and must not trigger the review pipeline (which
// would try to call GitHub with an empty token).
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  run().catch(async (err) => {
    const ctx = (() => {
      try { return loadConfig(process.env); } catch { return {}; }
    })();
    const log = makeLogger({
      prOwner: ctx.prOwner || "?",
      prRepo: ctx.prRepo || "?",
      prNumber: ctx.prNumber || "?",
      prHeadSha: ctx.prHeadSha || "?",
    });
    log.errlog("fatal", "boop runner failed", { error: String(err?.message ?? err) });
    const fatalOctokit = deps.getOctokit();
    if (fatalOctokit && ctx.statusCommentId) {
      await postStatus("failed", String(err?.message ?? err), ctx, {
        log: log.log,
        errlog: log.errlog,
        octokit: fatalOctokit,
      });
    }
    process.exit(1);
  });
}
