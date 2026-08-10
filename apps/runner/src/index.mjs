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
// `./lib/*.mjs` (config, log, security, git, openrouter, github,
// dashboard, workflow). Each lib module accepts a `ctx` (loaded
// config) and a `deps` bundle so the whole pipeline is
// unit-testable without env vars, real Octokit, real network, or
// real `git`.

import fs from "node:fs/promises";
import { randomUUID } from "node:crypto";
import jwt from "jsonwebtoken";
import { spawn } from "node:child_process";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import {
  loadConfig,
  REPO_DIR,
  CONFIG_SRC,
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
  postFinalReaction,
  readWorkflowState,
  writeWorkflowState,
} from "./lib/github.mjs";
import {
  postStatus as postDashboardStatus,
  postTelemetry,
  postLensTelemetry,
  startHeartbeat,
} from "./lib/dashboard.mjs";
import { createRtkAdapter } from "./lib/rtk.mjs";

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
  // QUB-103: per-run review id lets the runner find the existing
  // summary comment on retry and PATCH it instead of double-posting.
  // Generated before the handshake so postReview (called from the
  // `summary` macro-stage) can write the marker into the comment
  // body on the first POST. The same UUID is consumed by
  // postInlineComments for the dedup contract (each inline
  // carries its own per-(path,line,body-hash) marker; the review
  // id rides on the summary only).
  ctx.reviewId = randomUUID();
  const log = makeLogger(ctx);
  const cleanup = createCleanupRegistry({ errlog: log.errlog });

  // The Octokit instance has its own copy of the token inside, but
  // the local var lives in this scope. There is no secure way to
  // wipe a JS string in-place (V8 interns and may share the backing
  // buffer), so we instead limit the token's lifetime to the rest
  // of this function and rely on the netrc + gitconfig cleanup
  // hooks (see writeNetrc / writeGitconfig in lib/git.mjs) to make
  // any persistent copy unreachable.
  //
  // QUB-85: the rtk adapter wraps every file read the lenses do.
  // Constructed here (after `fs` and `execFile` are known) and
  // attached to `deps.rtk` so `buildBoopPrompt` and any future
  // lens can route reads through it. The adapter is the single
  // place rtk is invoked; the `BOOP_RTK_DISABLED=1` env var is
  // the operator kill switch.
  const rtkAdapter = createRtkAdapter({
    execFile: overrides.execFile ?? execFileAsync,
    env,
    fs: overrides.fs ?? fs,
    log: log.log,
    disabled: ctx.rtkDisabled,
  });
  const deps = {
    ...makeDeps(ctx, log, cleanup),
    ...overrides, // overrides.spawnFn, .execFile, .fetchImpl, .jwt, .fs
    rtk: rtkAdapter,
  };

  // Log the resolved adapter mode on startup so an operator can
  // confirm rtk is in the expected path without waiting for the
  // first file read.
  const rtkState = await rtkAdapter.init();
  log.log("rtk", "adapter initialised", {
    source: rtkState.source,
    binary: rtkState.binary,
    reason: rtkState.reason,
    disabled: ctx.rtkDisabled,
  });

  // QUB-109: 30s heartbeat so the receiver's stuck-runs
  // panel can distinguish a hung LLM call (heartbeats
  // arrive, stage never advances) from a crashed pod (no
  // heartbeats). Stop is wired into the finally block
  // below so a successful run, a failed run, and a
  // thrown error all clean up the timer. The unref inside
  // startHeartbeat keeps the timer from holding the
  // event loop open.
  const stopHeartbeat = startHeartbeat(ctx, deps);

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
    review_id: ctx.reviewId,
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
    //
    // SP-004 / DP-005: the run is created as "pending" in the
    // receiver. The receiver's live view filters by
    // store.StatusRunning; without an explicit transition here,
    // every in-flight review renders as "pending" and the live
    // panel is empty. The "running" post lands before any stage
    // starts (the stage POSTs run on a per-stage cadence) and
    // is fire-and-forget — a 4xx from a misconfigured
    // BOOP_DASHBOARD_TOKEN is surfaced at error level by
    // postWithRetry (EH-007).
    await postDashboardStatus("running", ctx, {
      log: log.log,
      errlog: log.errlog,
      fetchImpl: deps.fetchImpl,
    });
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
    //
    // QUB-131: even on a parseFailed run, post telemetry and
    // lens_telemetry if the narrator/lenses produced them. The
    // narrator ran successfully (only the summary gate tripped),
    // so the cost + token rollup is real. Without this, the
    // dashboard's cost column stays at zero for every soft
    // failure. Best-effort — failures inside the post helpers
    // are swallowed by postWithRetry.
    if (state.parseFailed) {
      await postRunnerTelemetryIfAny(state, ctx, deps);
      await postDashboardStatus(
        "failed",
        ctx,
        { log: log.log, fetchImpl: deps.fetchImpl },
        state.failureReason,
      );
      return;
    }

    // Dashboard data layer: POST final telemetry (token usage
    // + cost) once the review is fully posted. Best-effort —
    // failures are logged inside postTelemetry.
    //
    // QUB-95 + multi-expert: the multi-expert sub-workflow
    // makes N+1 LLM calls (1 walkthrough + N experts). The
    // final review's `state.review.telemetry` is the
    // narrator's call (the last one). The walkthrough's
    // and experts' telemetry lives on `state.walkthroughTelemetry`
    // and inside each expert's response. The dashboard
    // sums them on display; we POST the narrator's telemetry
    // as the primary row here. A future PR can roll the
    // per-stage telemetry into a single shape before posting.
    //
    // QUB-131: the helper is shared with the parseFailed branch
    // and the top-level catch (both reach a terminal state with
    // a usable telemetry row).
    await postRunnerTelemetryIfAny(state, ctx, deps);
    await postDashboardStatus("done", ctx, { log: log.log, fetchImpl: deps.fetchImpl });
    await deps.postStatus("done");
    // QUB-114: in reaction mode (issue_comment-triggered
    // re-review), the runner does not post or PATCH a status
    // comment. The author already saw the 👀 reaction the
    // receiver added; the runner adds a single terminal
    // reaction (🦴 on done, ❌ on failed) on the trigger
    // comment. The final-reaction call is best-effort; a
    // failure is logged and does not affect the run.
    if (ctx.noStatusComment) {
      await postFinalReaction("done", ctx, deps);
    }
  } catch (err) {
    log.errlog("review", "staged workflow failed", { error: String(err?.message ?? err) });
    // Best-effort dashboard status so the dashboard's live
    // view shows the run as failed even if the post-pipeline
    // status PATCH (deps.postStatus) also fails. The reason
    // mirrors the GitHub-commented reason so the operator's
    // primary view matches the source of truth.
    //
    // QUB-131: a stage throw (clone, sniff, narrate, etc.)
    // may have left a partial telemetry row behind. Forward
    // it so the dashboard still gets the cost rollup for the
    // LLM calls that did succeed before the throw.
    await postRunnerTelemetryIfAny(state, ctx, deps);
    const reason = state.failureReason || String(err?.message ?? err);
    await postDashboardStatus(
      "failed",
      ctx,
      { log: log.log, fetchImpl: deps.fetchImpl },
      reason,
    );
    await deps.postStatus("failed", reason);
    if (ctx.noStatusComment) {
      await postFinalReaction("failed", ctx, deps);
    }
    throw err;
  } finally {
    // QUB-109: stop the heartbeat before cleanup so a
    // pending POST doesn't race the credential scrub.
    stopHeartbeat();
    // Always scrub credentials and tmp artefacts, even on failure.
    // Order matters: revoke the netrc / gitconfig before unlinking
    // them so a future `git fetch` against the in-memory state
    // cannot read the token.
    await cleanup.runAll();
  }
}

// QUB-131: shared telemetry-poster for the three terminal
// states (success, parseFailed, top-level catch). The
// happy path, the parseFailed branch, and the catch all need
// to land the narrator's cost + token rollup so the dashboard's
// cost column is non-zero for soft failures. Both post helpers
// no-op when state has nothing to send, so this is safe to
// call unconditionally.
//
// `state.review.telemetry` is the narrator's row (the last LLM
// call in the multi-expert pipeline). `state.lensTelemetry` is
// the per-lens rollup the orchestrator builds during the
// walkthrough + experts stages. Both may be undefined on a
// run that died before the narrator (clone failure, etc.) —
// the guards below handle that case.
async function postRunnerTelemetryIfAny(state, ctx, deps) {
  if (state.review && state.review.telemetry) {
    await postTelemetry(state.review.telemetry, ctx, {
      log: deps.log,
      fetchImpl: deps.fetchImpl,
    });
  }
  // QUB-109: per-lens rollup. The runner accumulates one
  // entry per lens as the orchestrator emits `lens: <name>`
  // markers; this batch lands once at the end of the run.
  // (Previously this only fired on the happy path; on a
  // parseFailed run the lens attribution was already complete
  // but the batch never landed. QUB-131 unifies the paths so
  // soft failures still surface per-lens cost.)
  if (state.lensTelemetry && state.lensTelemetry.length > 0) {
    await postLensTelemetry(state.lensTelemetry, ctx, {
      log: deps.log,
      fetchImpl: deps.fetchImpl,
    });
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
    try {
      log.errlog("fatal", "boop runner failed", { error: String(err?.message ?? err) });
    } finally {
      process.exit(1);
    }
  });
}
