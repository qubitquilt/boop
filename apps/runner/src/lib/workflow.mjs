// Workflow stages.
//
// The runner today is one process that runs five macro steps in
// order (mint token, clone, sniff, summary, inlines, plus the
// re-review cleanup pass). This module pulls those steps into a
// data structure — STAGES — and a single `runStages` executor.
//
// The `sniff` macro-stage wraps a review sub-workflow. The
// sub-workflow is itself a list of sub-stages walked by
// `runSubWorkflow`. The sub-stages land incrementally:
//
//   - QUB-89: one placeholder sub-stage that calls the current
//     runOpenCodeSkill, so the sub-workflow is structurally
//     present but behavior is unchanged.
//   - QUB-94: classify (identify PR type)
//   - QUB-95: dispatch + gather + narrate (parallel experts)
//   - QUB-96: meta-review (bounded re-pass)
//
// Both executors share the gate / retry / resume machinery
// (QUB-90 / QUB-91 / QUB-92). The gate is the first piece: a
// per-stage precondition check that runs before `run`. A failed
// gate posts the "failed" status with the gate's reason and
// aborts the run; QUB-91 will introduce bounded retry on top.
//
// Each stage (macro or sub) has the same contract:
//
//   id          the workflow-internal name. Pinned by the
//               status-stage mapping and by the gate logic.
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
//   gate        async (state, ctx, deps) -> {ok: true} |
//               {ok: false, reason: "..."}. The executor calls
//               gate before run. A failed gate posts "failed"
//               and aborts the run.
//   run         the async function that does the work. Receives
//               (ctx, deps, state) and mutates `state` in
//               place. Throwing also aborts the run. The run
//               assumes the gate passed; the gate is the
//               contract for "this stage is allowed to run".

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
    gate: handshakeGate,
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
    gate: fetchGate,
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
    gate: sniffGate,
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
    gate: summaryGate,
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
    gate: inlinesGate,
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
    gate: cleanupGate,
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
    gate: sniffLegacyGate,
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

// runStages walks the macro-stage list in order. For each stage:
//   1. Call gate(state, ctx, deps). If it returns {ok: false},
//      log + post "failed" with the reason, set
//      state.parseFailed = true, and return. The orchestrator
//      in index.mjs checks state.parseFailed and short-circuits
//      cleanup / telemetry / done. The executor does NOT throw
//      on gate failure — a parse failure or a missing-input
//      failure is "expected" (the LLM might not produce a
//      structured block; we should not waste a Job exit on it).
//      QUB-91 (retry) will introduce bounded retry on top;
//      the retry's per-stage wrapper can check the gate's
//      return value directly without needing a throw.
//   2. Call run(ctx, deps, overrides, state). A throw is treated
//      as a hard failure and propagates to the orchestrator
//      catch in index.mjs (same as before the gate refactor).
//
// The gate + run split is the foundation QUB-91 (retry) and
// QUB-92 (resume) build on.
export async function runStages(ctx, deps, overrides, state) {
  for (const stage of STAGES) {
    const pre = await stage.gate(state, ctx, deps);
    if (!pre.ok) {
      await onGateFailure(stage.id, pre.reason, state, deps);
      return state;
    }
    await stage.run(ctx, deps, overrides, state);
  }
  return state;
}

// runSubWorkflow walks a sub-workflow stage list in order. Same
// shape as runStages (gate + run per stage). A gate failure
// inside the sub-workflow sets state.parseFailed and returns;
// the macro `sniff` stage's run sees the flag (via the next
// stage's gate) and short-circuits downstream.
export async function runSubWorkflow(stages, ctx, deps, overrides, state) {
  for (const stage of stages) {
    const pre = await stage.gate(state, ctx, deps);
    if (!pre.ok) {
      await onGateFailure(stage.id, pre.reason, state, deps);
      return state;
    }
    await stage.run(ctx, deps, overrides, state);
  }
}

// onGateFailure is the executor's response to a failed gate.
// It logs the failure, posts the "failed" status, and marks
// state.parseFailed so the orchestrator (and the cleanup stage)
// can short-circuit downstream work. The status message is the
// gate's reason verbatim, so a "summary parse failed: <X>"
// reason surfaces as a "summary parse failed: <X>" status line
// (matching the pre-QUB-90 user-visible surface).
async function onGateFailure(stageId, reason, state, deps) {
  deps.log("gate", `${stageId} gate failed`, { reason });
  await deps.postStatus("failed", reason);
  state.parseFailed = true;
}

// --- macro-stage gates --------------------------------------------------
//
// Each gate is a small async function. The gate is the
// precondition for the run; the run assumes the gate passed.
// The gate's `reason` is the verbatim string the executor
// surfaces in the "failed" status.

async function handshakeGate(_state, _ctx, _deps) {
  // handshake has no precondition; the run reads the secrets
  // and mints the token. A failure surfaces as a thrown error
  // from the run, not a gate failure.
  return { ok: true };
}

async function fetchGate(state, _ctx, _deps) {
  // fetch needs the Octokit (for the token) and the clone
  // path constants. Without the Octokit the clone would use a
  // stale token from a prior run; the gate catches that
  // misconfiguration at the boundary instead of mid-clone.
  if (!state.octokit) {
    return { ok: false, reason: "handshake did not populate state.octokit" };
  }
  return { ok: true };
}

async function sniffGate(_state, _ctx, _deps) {
  // sniff (the macro stage) needs the opencode API key. The
  // sub-workflow executor is responsible for surfacing
  // sub-stage gate failures; this gate is the boundary check
  // before the sub-workflow starts.
  return { ok: true };
}

async function summaryGate(state, _ctx, _deps) {
  // Summary is the first stage that consumes the review
  // output. The original inline code did this check too, in
  // the summary run, with a "summary parse failed: <reason>"
  // status message. The gate moves the check to the boundary
  // and preserves the message verbatim so the user-visible
  // surface is identical.
  if (!state.review) {
    return { ok: false, reason: "no review produced" };
  }
  if (!state.review.summary) {
    const reason = state.review.parseError || "summary empty";
    return { ok: false, reason: `summary parse failed: ${reason}` };
  }
  return { ok: true };
}

async function inlinesGate(state, _ctx, _deps) {
  // Inlines also needs a real review. On a parse failure the
  // summary gate has already aborted; this gate is a defense-
  // in-depth check in case a future caller runs the
  // sub-workflow out of order.
  if (!state.review || !state.review.summary) {
    return { ok: false, reason: "no review summary; nothing to post" };
  }
  return { ok: true };
}

async function cleanupGate(state, ctx, _deps) {
  // Cleanup is best-effort. The gate only short-circuits when
  // there is no review to clean up after (parse failure or no
  // review at all). The first-review / no-botLogin skip lives
  // in the run (it's not a failure, just a no-op).
  if (!state.review || !state.review.summary) {
    return { ok: false, reason: "no review landed; nothing to clean up" };
  }
  return { ok: true };
}

// --- macro-stage functions ----------------------------------------------

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
  if (typeof deps.setOctokit === "function") {
    deps.setOctokit(state.octokit);
  }
  deps.log("auth", "minted installation token");
  await deps.postStatus("auth");
}

async function fetchStage(ctx, deps, _overrides, _state) {
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
  // The summary gate (summaryGate) has already verified
  // state.review.summary is non-empty. Just call postReview.
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
  // The inlines gate (inlinesGate) has already verified
  // state.review.summary is non-empty.
  await postInlineComments(state.octokit, state.review.inlineComments, ctx, {
    log: deps.log,
    errlog: deps.errlog,
  });
}

async function cleanupStage(ctx, deps, overrides, state) {
  // Best-effort. The cleanup gate has already verified there
  // is a real review to clean up after. The remaining skips
  // (first review, no botLogin) are no-ops, not failures.
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
    deps.log("cleanup", "prior-review cleanup failed", {
      err: String(err?.message ?? err),
    });
    state.cleanup = { resolved: 0, minimized: 0, errors: 1 };
  }
}

// --- sub-stage gate + function -----------------------------------------

async function sniffLegacyGate(state, _ctx, _deps) {
  // Placeholder sub-stage needs the opencode key + the cloned
  // repo (deps.paths.repoDir). The macro fetch stage has
  // already populated /work/repo; the handshake stage has
  // populated state.openrouterApiKey. The gate is a
  // defense-in-depth check.
  if (!state.openrouterApiKey) {
    return { ok: false, reason: "no openrouter api key" };
  }
  return { ok: true };
}

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
