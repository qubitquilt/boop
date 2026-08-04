// Workflow stages.
//
// The runner today is one process that runs five macro steps in
// order (mint token, clone, sniff, summary, inlines). This module
// pulls those steps into a data structure — STAGES — and a single
// `runStages` executor.
//
// The `sniff` macro-stage wraps a review sub-workflow. The
// sub-workflow is itself a list of sub-stages walked by
// `runSubWorkflow`. The sub-stages land incrementally:
//
//   - QUB-89 (this PR): one placeholder sub-stage that calls the
//     current runOpenCodeSkill, so the sub-workflow is structurally
//     present but behavior is unchanged.
//   - QUB-94: classify (identify PR type)
//   - QUB-95: dispatch + gather + narrate (parallel experts)
//   - QUB-96: meta-review (bounded re-pass)
//
// Both executors share the gate / retry / resume machinery that
// lands in QUB-90 / QUB-91 / QUB-92. Today the executors are plain
// `for await` loops; the wiring attaches in the later PRs.
//
// Each stage (macro or sub) has the same contract:
//
//   id          the workflow-internal name. Pinned by the
//               status-stage mapping and by the gate logic in
//               later PRs.
//   statusStage the status label that gets PATCHed to the GitHub
//               comment for this stage. null = silent (no new
//               status line). The user-visible surface is pinned
//               by QUB-93; do not change these without a
//               follow-up ticket.
//   description short prose so a future reader can scan the
//               table of contents without reading the code.
//   input       what the stage reads from `state` / `ctx`.
//   output      what the stage writes to `state`.
//   idempotent  whether re-running the stage is safe (re-posts
//               to the PR, re-clones, etc.). Drives the resume
//               logic in QUB-92.
//   run         the async function that does the work. Receives
//               (ctx, deps, state) and mutates `state` in
//               place. Throwing aborts the run.

import { cloneRepo } from "./git.mjs";
import { runOpenCodeSkill as defaultRunOpenCodeSkill } from "./opencode.mjs";
import {
  cleanupPriorReview as defaultCleanupPriorReview,
  makeOctokit,
  mintInstallationToken,
  postReview,
  postInlineComments,
} from "./github.mjs";
import { readSecretFile } from "./security.mjs";

export const STAGES = [
  {
    id: "handshake",
    statusStage: "auth",
    description:
      "Read the GitHub App private key + OpenRouter API key, mint an installation token, build the Octokit instance.",
    input: "ctx, mounted secret files",
    output:
      "state.installationToken, state.openrouterApiKey, state.octokit",
    idempotent: false,
    run: handshakeStage,
  },
  {
    id: "fetch",
    statusStage: "clone",
    description:
      "Clone the PR at the head SHA into /work/repo, with the installation token via a short-lived netrc + gitconfig.",
    input: "ctx, state.octokit (for the token)",
    output: "/work/repo populated at PR_HEAD_SHA",
    idempotent: false,
    run: fetchStage,
  },
  {
    id: "sniff",
    statusStage: "review",
    description:
      "Run the review sub-workflow. Today this is a single sub-stage (sniff-legacy) that calls the existing runOpenCodeSkill; QUB-94 through QUB-96 expand it into classify / dispatch / gather / meta-review / narrate.",
    input: "ctx, state.openrouterApiKey, /work/repo",
    output: "state.review (and state.findings, state.classification as the sub-stages land)",
    idempotent: false,
    run: sniffStage,
  },
  {
    id: "summary",
    statusStage: null,
    description:
      "Post the summary as a single PR comment with the head-SHA marker for re-review diffing.",
    input: "state.review.summary, state.review.confidence, ctx",
    output: "a new PR comment with the summary + head-SHA marker",
    idempotent: false,
    run: summaryStage,
  },
  {
    id: "inlines",
    statusStage: null,
    description:
      "Post each line-specific comment as a review comment on the diff.",
    input: "state.review.inlineComments, ctx, state.octokit",
    output: "N review comments on the PR",
    idempotent: false,
    run: inlinesStage,
  },
  {
    id: "cleanup",
    statusStage: null,
    description:
      "On re-reviews with a known bot login, resolve outdated Boop review threads and minimize prior Boop issue comments so the PR thread is dominated by the active review. Skipped on the first review and when botLogin is unset.",
    input: "state.installationToken, ctx, state.botLogin",
    output: "state.cleanup = { resolved, minimized, errors } (or null on skip)",
    idempotent: true,
    run: cleanupStage,
  },
];

// REVIEW_SUB_STAGES is the list of sub-stages inside the `sniff`
// macro-stage. Today it has one placeholder sub-stage that calls
// the existing runOpenCodeSkill; the sub-workflow is structurally
// present (a `runSubWorkflow` executor walks it) so the later
// QUB-94 / QUB-95 / QUB-96 PRs only need to push entries onto
// the list, not introduce the executor.
//
// Sub-stages are silent on the status thread (statusStage: null).
// The "review" status line is posted once at the start of the
// macro `sniff` stage and covers the whole sub-workflow (so
// QUB-93's user-visible surface stays the same).
export const REVIEW_SUB_STAGES = [
  {
    id: "sniff-legacy",
    statusStage: null,
    description:
      "Placeholder sub-stage that calls the existing runOpenCodeSkill. QUB-95 replaces this with classify / dispatch / gather / narrate.",
    input: "ctx, state.openrouterApiKey, /work/repo",
    output: "state.review",
    idempotent: false,
    run: sniffLegacySubStage,
  },
];

// statusStageFor maps a macro-stage id to the status label the
// runner PATCHes to the GitHub comment. Returns null for silent
// stages (summary, inlines) and for unknown ids. Sub-stages do
// not have status labels; their parent macro-stage posts the
// single "review" line.
export function statusStageFor(id) {
  const stage = STAGES.find((s) => s.id === id);
  return stage ? stage.statusStage : null;
}

// runStages walks the macro-stage list in order, calling each
// stage's `run` function. The `state` object is shared between
// stages and accumulates their outputs. The `overrides` object
// lets a test inject a custom runOpenCodeSkill, cloneRepo, or
// makeOctokit — the same hook the inline run() in index.mjs used
// before this refactor.
//
// On success, returns the final state. On any stage throwing,
// the error propagates; the caller (run() in index.mjs) is
// responsible for posting the "failed" status.
//
// Note: the executor intentionally has no gate or retry logic
// yet. Those land in QUB-90 (gates) and QUB-91 (retry). The
// current behavior is identical to the inline pipeline that
// existed before this refactor; the split is purely structural.
export async function runStages(ctx, deps, overrides, state) {
  for (const stage of STAGES) {
    await stage.run(ctx, deps, overrides, state);
  }
  return state;
}

// runSubWorkflow walks a sub-workflow stage list in order. Same
// signature and override surface as runStages; used by the
// `sniff` macro-stage to walk REVIEW_SUB_STAGES. The shape is
// the same as runStages so the gate / retry / resume machinery
// (QUB-90 / QUB-91 / QUB-92) attaches to both with one helper.
//
// The executor is intentionally a plain for-loop today. The
// meta-review sub-stage (QUB-96) will introduce a bounded
// re-pass loop on top of this executor.
export async function runSubWorkflow(stages, ctx, deps, overrides, state) {
  for (const stage of stages) {
    await stage.run(ctx, deps, overrides, state);
  }
}

// --- macro-stage functions ----------------------------------------------
//
// Each stage is a small function. The `overrides` hook is the
// same one the inline run() used; preserved verbatim so the
// existing test suite (which stubs runOpenCodeSkill, cloneRepo,
// and makeOctokit) keeps working without changes.

async function handshakeStage(ctx, deps, overrides, state) {
  const GITHUB_APP_PRIVATE_KEY = await readSecretFile(
    "GITHUB_APP_PRIVATE_KEY",
    ctx.privateKeyPath,
    deps.fs,
  );
  const OPENROUTER_API_KEY = await readSecretFile(
    "OPENROUTER_API_KEY",
    ctx.openrouterKeyPath,
    deps.fs,
  );
  const installationToken = await mintInstallationToken(
    ctx.githubAppId,
    GITHUB_APP_PRIVATE_KEY,
    ctx.githubAppInstallationId,
    deps,
  );
  state.installationToken = installationToken;
  state.openrouterApiKey = OPENROUTER_API_KEY;
  state.octokit = overrides.makeOctokit
    ? overrides.makeOctokit(installationToken)
    : makeOctokit(installationToken);
  // Write the octokit to the deps slot so the postStatus closure
  // (which is created before the handshake stage runs) can read it
  // when PATCHing the status comment.
  if (typeof deps.setOctokit === "function") {
    deps.setOctokit(state.octokit);
  }
  deps.log("auth", "minted installation token");
  await deps.postStatus("auth");
}

async function fetchStage(ctx, deps, _overrides, _state) {
  // cloneRepo is wrapped in makeDeps (index.mjs) so the postStatus
  // call inside it routes through the lazy-octokit resolver. The
  // test fixture overrides deps.cloneRepo directly.
  await deps.cloneRepo(ctx, deps);
  deps.log("clone", "repo cloned", {
    dir: deps.paths.repoDir,
    sha: ctx.prHeadSha,
  });
}

async function sniffStage(ctx, deps, overrides, state) {
  // Macro stage: walk the sub-workflow. Today the sub-workflow
  // is a single placeholder sub-stage that calls the existing
  // runOpenCodeSkill, so behavior is unchanged from the inline
  // pipeline. The sub-workflow shape exists so QUB-94 / QUB-95 /
  // QUB-96 can push sub-stages onto the list without changing
  // the macro executor.
  await runSubWorkflow(REVIEW_SUB_STAGES, ctx, deps, overrides, state);
}

async function summaryStage(ctx, deps, _overrides, state) {
  if (!state.review || !state.review.summary) {
    // Parse failure or empty review. The sniff stage already
    // logged the reason; the summary gate (QUB-90) is where this
    // will be enforced. For now the inline behavior matches
    // the pre-refactor code path: log, post failed, and let the
    // orchestrator short-circuit. The caller in index.mjs checks
    // state.review.summary and skips summary + inlines.
    deps.log("done", "summary parse failed, skipping post", {
      reason: state.review ? state.review.parseError : "no review",
      confidence: state.review ? state.review.confidence : "low",
    });
    await deps.postStatus(
      "failed",
      `summary parse failed: ${(state.review && state.review.parseError) || "unknown"}`,
    );
    // Mark the state so index.mjs can short-circuit cleanly.
    state.parseFailed = true;
    return;
  }
  await postReview(
    state.octokit,
    state.review.summary,
    ctx.reviewNumber,
    state.review.confidence,
    ctx,
  );
  deps.log("done", "summary comment posted", {
    review_number: ctx.reviewNumber,
    confidence: state.review.confidence,
  });
}

async function inlinesStage(ctx, deps, _overrides, state) {
  if (!state.review || !state.review.summary) {
    // Mirror the summary short-circuit so a parse-failed run
    // never reaches the inline posts.
    return;
  }
  await postInlineComments(state.octokit, state.review.inlineComments, ctx, {
    log: deps.log,
    errlog: deps.errlog,
  });
}

async function cleanupStage(ctx, deps, overrides, state) {
  // The cleanup pass runs on re-reviews with a known bot login.
  // First review (reviewNumber === 1) has no prior artifacts to
  // retire. When botLogin is unset, the receiver didn't tell us
  // who the bot is and we have no way to identify the prior
  // comments. When the sniff stage produced a parse failure
  // (state.parseFailed), the new review never landed; the prior
  // review is still the current one on the PR, so retiring its
  // artifacts would be wrong. All three paths short-circuit; the
  // gate (QUB-90) will formalize the check.
  if (state.parseFailed) {
    deps.log("cleanup", "skipped (parse failure, no new review landed)");
    state.cleanup = null;
    return;
  }
  if (ctx.reviewNumber <= 1 || !ctx.botLogin) {
    state.cleanup = null;
    return;
  }
  try {
    const cleanupFn =
      overrides.cleanupPriorReview || defaultCleanupPriorReview;
    const cleaned = await cleanupFn(state.installationToken, ctx, deps);
    state.cleanup = cleaned;
    if (cleaned.resolved > 0 || cleaned.minimized > 0) {
      deps.log("cleanup", "retired prior review artifacts", cleaned);
    } else {
      deps.log("cleanup", "no prior artifacts to retire");
    }
  } catch (err) {
    // Best-effort: a cleanup failure is logged but does not
    // fail the run. The new review is already posted.
    deps.log("cleanup", "prior-review cleanup failed", {
      err: String(err?.message ?? err),
    });
    state.cleanup = { resolved: 0, minimized: 0, errors: 1 };
  }
}

// --- sub-stage functions ------------------------------------------------

async function sniffLegacySubStage(ctx, deps, overrides, state) {
  // Placeholder sub-stage that calls the existing runOpenCodeSkill.
  // QUB-95 will replace this with the multi-expert sub-workflow
  // (classify / dispatch / gather / narrate).
  const skillFn = overrides.runOpenCodeSkill || defaultRunOpenCodeSkill;
  state.review = await skillFn(state.openrouterApiKey, ctx, deps);
  const telemetry = state.review.telemetry ?? null;
  deps.log("review", "opencode returned", {
    summaryBytes: state.review.summary ? state.review.summary.length : 0,
    inlineCount: state.review.inlineComments
      ? state.review.inlineComments.length
      : 0,
    confidence: state.review.confidence,
    ...(telemetry
      ? {
          cost_usd: telemetry.costUsd,
          tokens_in: telemetry.inputTokens,
          tokens_out: telemetry.outputTokens,
          step_count: telemetry.stepCount,
        }
      : {}),
  });
}
