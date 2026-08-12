// Boop runner.
//
// Orchestrates one PR review:
//   1. Reads the GitHub App private key and OpenRouter API key from
//      their mounted Secret files (not env — env is visible to a
//      prompt-injected LLM via /proc/self/environ).
//   2. Walks the staged workflow (handshake → fetch → sniff →
//      summary → inlines) defined in `./lib/workflow.ts`.
//   3. PATCHes the status comment at each stage with the latest
//      emoji + message.
//   4. Posts the review body to the PR as a single comment, plus
//      the line-specific inline comments.
//
// Structure: this file is the orchestrator. The work is in
// `./lib/*.ts` (config, log, security, git, openrouter, github,
// dashboard, workflow). Each lib module accepts a `ctx` (loaded
// config) and a `deps` bundle so the whole pipeline is
// unit-testable without env vars, real Octokit, real network, or
// real `git`.

import fs from "node:fs/promises";
import { randomUUID } from "node:crypto";
import jwt from "jsonwebtoken";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import {
  loadConfig,
  NETRC_PATH,
  GITCONFIG_PATH,
} from "./lib/config.ts";
import { makeLogger } from "./lib/log.ts";
import { OctokitSlot, StatusCommentSlot } from "./lib/slots.ts";
import { assertSafeRef, assertSafeSha } from "./lib/security.ts";
import { createCleanupRegistry, cloneRepo } from "./lib/git.ts";
import { runStages } from "./lib/workflow.ts";
import {
  postStatus,
  ensureStatusComment,
  postFinalReaction,
  readWorkflowState,
  writeWorkflowState,
} from "./lib/github.ts";
import {
  postStatus as postDashboardStatus,
  postTelemetry,
  postLensTelemetry,
  startHeartbeat,
} from "./lib/dashboard.ts";
import { createRtkAdapter } from "./lib/rtk.ts";
import type { Ctx, Deps, FetchLike, OctokitLike, Overrides, State } from "./types.ts";

const execFileAsync = promisify(execFile);

type RunOverrides = Overrides & {
  fs?: typeof fs;
  execFile?: typeof execFileAsync;
  fetchImpl?: FetchLike;
  jwt?: typeof jwt;
  spawnFn?: typeof spawn;
  [key: string]: unknown;
};

function makeDeps(
  ctx: Ctx,
  log: { log: (stage: string, msg: string, extra?: Record<string, unknown>) => void; errlog: (stage: string, msg: string, extra?: Record<string, unknown>) => void },
  cleanup: ReturnType<typeof createCleanupRegistry>,
): Deps {
  const octokitSlot = new OctokitSlot();
  const statusCommentSlot = new StatusCommentSlot(ctx.statusCommentId);
  const patchStatusWithCtx = (stage: string, detail?: string) =>
    postStatus(stage, detail, statusCommentSlot.applyTo(ctx), {
      log: log.log,
      errlog: log.errlog,
      octokit: octokitSlot.get(),
    });
  const postStatusWrapper = async (stage: string, detail?: string) => {
    if (!octokitSlot.isReady()) {
      log.log("status", "skip (no octokit yet)", { stage });
      return;
    }
    await ensureStatusComment(
      octokitSlot.get(),
      ctx,
      { log: log.log, errlog: log.errlog },
      statusCommentSlot as unknown as { value: number | null },
      ctx.triggeredBy || null,
    );
    await patchStatusWithCtx(stage, detail);
  };
  return {
    fs: fs as unknown as Deps["fs"],
    execFile: execFileAsync,
    spawnFn: spawn,
    jwt: jwt as unknown as Deps["jwt"],
    fetchImpl: fetch as unknown as FetchLike,
    fetch: fetch as unknown as FetchLike,
    OctokitCtor: undefined,
    env: { OPENROUTER_API_KEY: "" } as Deps["env"],
    paths: {
      repoDir: ctx.repoDir,
      configSrc: ctx.configSrc,
      netrc: NETRC_PATH,
      gitconfig: GITCONFIG_PATH,
    },
    cleanup: cleanup as unknown as Deps["cleanup"],
    log: log.log,
    errlog: log.errlog,
    setOctokit: (octokit: OctokitLike | null) => octokitSlot.set(octokit),
    getOctokit: () => octokitSlot.get(),
    currentCtx: () => statusCommentSlot.applyTo(ctx),
    postStatus: postStatusWrapper,
    cloneRepo: (c, d) =>
      cloneRepo(c as Ctx & { installationToken: string; home: string }, {
        ...d,
        postStatus: (s) => postStatusWrapper(s, undefined),
      } as Parameters<typeof cloneRepo>[1]),
  };
}

export async function run(
  env: NodeJS.ProcessEnv = process.env,
  overrides: RunOverrides = {},
): Promise<void> {
  const ctx: Ctx = loadConfig(env);
  ctx.reviewId = randomUUID();
  const log = makeLogger(ctx);
  const cleanup = createCleanupRegistry({ errlog: log.errlog });

  const rtkAdapter = createRtkAdapter({
    execFile: (overrides.execFile as typeof execFileAsync) ?? execFileAsync,
    env,
    fs: (overrides.fs as unknown as typeof fs) ?? (fs as unknown as typeof fs),
    log: log.log,
    disabled: ctx.rtkDisabled,
  } as unknown as Parameters<typeof createRtkAdapter>[0]);
  const deps: Deps = {
    ...makeDeps(ctx, log, cleanup),
    ...(overrides as Partial<Deps>),
    rtk: rtkAdapter,
  };

  const rtkState = await rtkAdapter.init();
  log.log("rtk", "adapter initialised", {
    source: rtkState.source,
    binary: rtkState.binary,
    reason: rtkState.reason,
    disabled: ctx.rtkDisabled,
  });

  const stopHeartbeat = startHeartbeat(ctx, deps);

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

  const state: State = { passed: [], sub: {} };

  let stateInitialized = false;
  const onStagePassed = async (stageId: string): Promise<void> => {
    const octokit = deps.getOctokit();
    if (!octokit) return;
    const liveCtx = deps.currentCtx();
    if (!liveCtx.statusCommentId) return;
    if (!stateInitialized) {
      const prior = await readWorkflowState(octokit, liveCtx, {
        log: deps.log,
        errlog: deps.errlog,
      });
      if (prior.passed && prior.passed.length > 0) {
        state.passed = [
          ...prior.passed.filter((id) => !(state.passed || []).includes(id)),
          ...(state.passed || []),
        ];
      }
      if (prior.sub && Object.keys(prior.sub).length > 0) {
        for (const [macroId, subs] of Object.entries(prior.sub)) {
          const subArr = subs as string[];
          state.sub = state.sub || {};
          const existing = state.sub[macroId] || [];
          state.sub[macroId] = [
            ...subArr.filter((id) => !existing.includes(id)),
            ...existing,
          ];
        }
      }
      if ((state.passed || []).length > 0 || Object.keys(state.sub || {}).length > 0) {
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
      { log: deps.log, errlog: deps.errlog },
      { passed: state.passed || [], sub: state.sub || {} },
    );
  };

  try {
    await postDashboardStatus("running", ctx, {
      log: deps.log,
      errlog: deps.errlog,
      fetchImpl: deps.fetchImpl,
    });
    await runStages(ctx, deps, overrides, state, {
      onStagePassed: (stageId) => onStagePassed(stageId),
    });

    if (state.parseFailed) {
      await postRunnerTelemetryIfAny(state, ctx, deps);
      await postDashboardStatus(
        "failed",
        ctx,
        { log: deps.log, errlog: deps.errlog, fetchImpl: deps.fetchImpl },
        state.failureReason,
      );
      return;
    }

    await postRunnerTelemetryIfAny(state, ctx, deps);
    await postDashboardStatus("done", ctx, { log: deps.log, errlog: deps.errlog, fetchImpl: deps.fetchImpl });
    await deps.postStatus("done");
    if (ctx.noStatusComment) {
      await postFinalReaction("done", ctx, deps);
    }
  } catch (err) {
    log.errlog("review", "staged workflow failed", { error: String(err instanceof Error ? err.message : err) });
    await postRunnerTelemetryIfAny(state, ctx, deps);
    const reason = state.failureReason || String(err instanceof Error ? err.message : err);
    await postDashboardStatus(
      "failed",
      ctx,
      { log: deps.log, errlog: deps.errlog, fetchImpl: deps.fetchImpl },
      reason,
    );
    await deps.postStatus("failed", reason);
    if (ctx.noStatusComment) {
      await postFinalReaction("failed", ctx, deps);
    }
    throw err;
  } finally {
    stopHeartbeat();
    await cleanup.runAll();
  }
}

async function postRunnerTelemetryIfAny(state: State, ctx: Ctx, deps: Deps): Promise<void> {
  if (state.review && state.review.telemetry) {
    await postTelemetry(state.review.telemetry, ctx, {
      log: deps.log,
      errlog: deps.errlog,
      fetchImpl: deps.fetchImpl,
    });
  }
  if (state.lensTelemetry && state.lensTelemetry.length > 0) {
    await postLensTelemetry(state.lensTelemetry, ctx, {
      log: deps.log,
      errlog: deps.errlog,
      fetchImpl: deps.fetchImpl,
    });
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  run().catch(async (err) => {
    const ctx = (() => {
      try { return loadConfig(process.env); } catch { return {} as Partial<Ctx>; }
    })();
    const log = makeLogger({
      prOwner: ctx.prOwner || "?",
      prRepo: ctx.prRepo || "?",
      prNumber: ctx.prNumber || "?",
      prHeadSha: ctx.prHeadSha || "?",
    });
    try {
      log.errlog("fatal", "boop runner failed", { error: String(err instanceof Error ? err.message : err) });
    } finally {
      process.exit(1);
    }
  });
}
