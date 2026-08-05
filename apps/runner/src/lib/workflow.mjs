// Workflow stages.
//
// The runner today is one process that runs five macro steps in
// order (mint token, clone, sniff, summary, inlines, plus the
// re-review cleanup pass). This module pulls those steps into a
// data structure — STAGES — and a single `runStages` executor.
//
// The `sniff` macro-stage wraps a review sub-workflow. The
// sub-workflow is itself a list of sub-stages walked by
// runSubWorkflow. The sub-stages land incrementally:
//
//   - QUB-89: one placeholder sub-stage that calls the current
//     runOpenCodeSkill, so the sub-workflow is structurally
//     present but behavior is unchanged.
//   - QUB-94: classify (identify PR type)
//   - QUB-95: dispatch + gather + narrate (parallel experts)
//   - QUB-96: meta-review (bounded re-pass)
//
// Each macro and sub stage has the same contract:
//
//   id          the workflow-internal name. Pinned by the
//               status-stage mapping and by the gate logic.
//   statusStage the status label that gets PATCHed to the GitHub
//               comment. null = silent. The user-visible surface
//               is pinned by QUB-93.
//   description short prose.
//   input       what the stage reads.
//   output      what the stage writes.
//   idempotent  whether re-running is safe. Drives QUB-92.
//   retryable   whether a gate failure or run-time error is
//               worth retrying. Default true; parse failure
//               and auth-style stages set false.
//   gate        async (state, ctx, deps) -> {ok: true} |
//               {ok: false, reason: "..."}. The executor
//               calls gate before run. A failed gate posts
//               "failed" and aborts the run (after retries,
//               see QUB-91).
//   run         async (ctx, deps, overrides, state) -> void.
//               Mutates state. Throwing aborts the run (after
//               retries if retryable).
//
// The executors (runStages, runSubWorkflow) walk the stage list
// with bounded retry: a failed gate or a thrown run retries up
// to N times with exponential backoff. The retry policy lives
// in deps (stageMaxAttempts, stageBackoffBaseMs,
// stageBackoffMaxMs) so a test can collapse it to one attempt
// and exercise the failure path immediately.

import { setTimeout as defaultSleep } from "node:timers/promises";
import { defaultClassify } from "./classify.mjs";
import {
  defaultMetaReview,
  gather,
  mergeByExpert,
  pickExperts,
  runExperts,
} from "./experts.mjs";
import { cloneRepo } from "./git.mjs";
import { runOpenCodeSkill as defaultRunOpenCodeSkill } from "./openrouter.mjs";
import {
  cleanupPriorReview as defaultCleanupPriorReview,
  makeOctokit,
  mintInstallationToken,
  postReview,
  postInlineComments,
} from "./github.mjs";
import { readSecretFile } from "./security.mjs";

// Default retry policy. The receiver passes overrides via the
// runner's env (BOOP_STAGE_MAX_ATTEMPTS, BOOP_STAGE_BACKOFF_BASE_MS,
// BOOP_STAGE_BACKOFF_MAX_MS) once QUB-91's follow-up lands;
// loadConfig will pick them up. For now the defaults apply.
// Exported so workflow.test.mjs can assert on the exact values.
export const DEFAULT_MAX_ATTEMPTS = 3;
export const DEFAULT_BACKOFF_BASE_MS = 1000;
export const DEFAULT_BACKOFF_MAX_MS = 30000;

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
    retryable: false, // auth-style: a retry will hit the same broken creds
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
    retryable: true, // network blip on a clone is worth a retry
    gate: fetchGate,
    run: fetchStage,
  },
  {
    id: "sniff",
    statusStage: "review",
    description:
      "Run the review sub-workflow. Today a single sub-stage (sniff-legacy) that calls the existing runOpenCodeSkill; QUB-94 through QUB-96 expand it into classify / dispatch / gather / meta-review / narrate.",
    input: "ctx, state.openrouterApiKey, /work/repo",
    output: "state.review (and state.findings, state.classification as the sub-stages land)",
    idempotent: false,
    retryable: true, // LLM calls + clone can be transient
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
    retryable: false, // the gate is the summary gate; a parse failure isn't transient
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
    retryable: true, // GitHub rate-limit / 5xx is worth a retry
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
    retryable: true,
    gate: cleanupGate,
    run: cleanupStage,
  },
];

// REVIEW_SUB_STAGES is the list of sub-stages inside the `sniff`
// macro-stage. QUB-95 replaced the sniff-legacy placeholder
// with classify -> dispatch -> gather -> narrate. QUB-96
// inserts a meta-review sub-stage between gather and
// narrate: the meta-reviewer scans the gathered findings
// for things that "stick out as potentially wrong" and
// requests a bounded re-pass of the specific experts that
// produced those findings. Bounded to one re-pass per run
// (the meta-reviewer cannot re-loop).
//
// Sub-stages are silent on the status thread (statusStage: null).
// The "review" status line is posted once at the start of the
// macro `sniff` stage and covers the whole sub-workflow (so
// QUB-93's user-visible surface stays the same).
export const REVIEW_SUB_STAGES = [
  {
    id: "classify",
    statusStage: null,
    description:
      "Identify the PR type (bug fix, feature, refactor, docs, test-only, infra) to drive the expert selection in dispatch.",
    input: "ctx, state.openrouterApiKey, /work/repo",
    output: "state.classification = { type, confidence }",
    idempotent: false,
    retryable: true,
    gate: classifyGate,
    run: classifySubStage,
  },
  {
    id: "dispatch",
    statusStage: null,
    description:
      "Pick the experts for this PR type and run them in parallel. Each expert is an independent LLM call with a tailored prompt + tool surface (bash, file reads, test runner) scoped to the cloned repo.",
    input: "state.classification, /work/repo",
    output: "state.findings = [Finding, ...]",
    idempotent: false,
    retryable: true,
    gate: dispatchGate,
    run: dispatchSubStage,
  },
  {
    id: "gather",
    statusStage: null,
    description:
      "De-duplicate and flatten the expert findings into a single list the meta-reviewer + narrator can consume.",
    input: "state.findings",
    output: "state.findings (de-duped in place)",
    idempotent: true,
    retryable: true,
    gate: gatherGate,
    run: gatherSubStage,
  },
  {
    id: "meta-review",
    statusStage: null,
    description:
      "Scan the gathered findings for things that 'stick out as potentially wrong' (false positives, contradictions, missing context). If anything sticks out, request a bounded re-pass of the specific experts that produced those findings. Bounded to one re-pass per run.",
    input: "state.findings, state.classification",
    output: "state.findings (with re-passed expert findings replaced in place)",
    idempotent: false,
    retryable: true,
    gate: metaReviewGate,
    run: metaReviewSubStage,
  },
  {
    id: "narrate",
    statusStage: null,
    description:
      "Produce the cohesive summary + inline-comment set from the (possibly re-reviewed) findings. The output shape is identical to the single-LLM-call path so the downstream summary + inlines stages are unaffected.",
    input: "state.findings, ctx, /work/repo",
    output: "state.review = { summary, inlineComments, confidence, telemetry }",
    idempotent: false,
    retryable: true,
    gate: narrateGate,
    run: narrateSubStage,
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

// runStages walks the macro-stage list in order. Each stage is
// invoked through withRetry, which applies the bounded-attempt /
// exponential-backoff policy. A failed gate (after retries) is
// "soft" — the executor sets state.parseFailed and returns; the
// orchestrator in index.mjs short-circuits the lifecycle. A
// thrown run (after retries) is "hard" — it propagates to the
// orchestrator catch.
//
// QUB-92 + QUB-102: the executor honors state.passed (an array
// of macro stage ids) read from the status comment by the
// orchestrator in index.mjs. A macro stage whose id is in
// state.passed means a prior pod of the same Job already ran
// it (the comment write fires from onStagePassed after every
// passing stage). QUB-92 used this as a resume signal (skip
// and continue); QUB-102 flips it to an abort signal so the
// current pod cannot post a duplicate summary + inline
// comments when a sibling pod of the same Job has already
// done so. With BackoffLimit=0 in the Job spec the K8s
// controller will not auto-restart pods, so this path is the
// belt-and-suspenders defense for manual re-triggers / K8s
// bugs / etc.
//
// The abort is "soft" — same shape as a failed gate. We post
// the failed status with the reason verbatim and set
// state.parseFailed; the orchestrator in index.mjs short-
// circuits the lifecycle so no summary, no inlines, and no
// cleanup stage run on the second pod.
//
// options.onStagePassed is an optional callback the executor
// fires after every stage that passes. The orchestrator uses
// it to persist state to the status comment so a pod kill
// mid-run does not lose progress.
export async function runStages(ctx, deps, overrides, state, options = {}) {
  const onStagePassed = options.onStagePassed || (() => {});
  for (const stage of STAGES) {
    if (state.passed && state.passed.includes(stage.id)) {
      // QUB-102: another pod of this Job already passed this
      // stage. Abort the current run to prevent duplicate
      // GitHub side effects (summary comment, inline review
      // threads) — see jobbuilder.go:59 (jobBackoffLimit=0)
      // for the primary defense.
      //
      // The reason lists the full passed set so the operator
      // sees the prior pod's progress at a glance from the
      // status timeline. The merge in index.mjs runs after
      // the per-iteration push, so the array order can shift
      // (e.g. ["fetch","handshake"] instead of
      // ["handshake","fetch"]); the items are the same, the
      // order is cosmetic. The orchestrator reads
      // state.failureReason and forwards it to the dashboard
      // so the operator's primary view is not just "failed"
      // with no context.
      const reason = `another pod already passed [${state.passed.join(", ")}]; refusing to duplicate the review`;
      deps.errlog("abort", reason, {
        stage: stage.id,
        passed: state.passed,
      });
      state.failureReason = reason;
      await deps.postStatus("failed", reason);
      state.parseFailed = true;
      return state;
    }
    const wasPassed = state.parseFailed;
    await withRetry(stage, ctx, deps, overrides, state);
    if (state.parseFailed && !wasPassed) return state;
    if (state.parseFailed) return state;
    // The stage passed. Append to state.passed and notify.
    state.passed = state.passed || [];
    if (!state.passed.includes(stage.id)) state.passed.push(stage.id);
    await onStagePassed(stage.id, state);
  }
  return state;
}

// runSubWorkflow walks a sub-workflow stage list in order. Same
// shape as runStages. Honors state.sub[macroId] (a list of
// sub-stage ids) for the macro stage's sub-workflow; the
// orchestrator in index.mjs is responsible for setting
// state.sub[macroId] when reading the workflow state. A
// sub-stage whose id is in the per-macro list is skipped.
export async function runSubWorkflow(stages, ctx, deps, overrides, state) {
  const macroId = state._subWorkflowOf;
  const subPassed =
    macroId && state.sub && Array.isArray(state.sub[macroId])
      ? state.sub[macroId]
      : null;
  for (const stage of stages) {
    if (subPassed && subPassed.includes(stage.id)) {
      deps.log("resume", `sub-stage ${stage.id} already passed; skipping`, {
        sub: subPassed,
      });
      continue;
    }
    const wasPassed = state.parseFailed;
    await withRetry(stage, ctx, deps, overrides, state);
    if (state.parseFailed && !wasPassed) return state;
    if (state.parseFailed) return state;
    // Sub-stage passed. Append to state.sub[macroId] if we know
    // the macro id. The orchestrator's onStagePassed callback
    // (fired by the macro executor after the macro stage
    // returns) will pick up the updated sub list and persist
    // the state.
    if (macroId) {
      state.sub = state.sub || {};
      state.sub[macroId] = state.sub[macroId] || [];
      if (!state.sub[macroId].includes(stage.id)) {
        state.sub[macroId].push(stage.id);
      }
    }
  }
}

// withRetry applies the bounded-attempt / exponential-backoff
// policy to a single stage. The shape:
//
//   1. Call gate(state, ctx, deps).
//   2. If gate returns {ok: false} AND the stage is retryable
//      AND there are attempts left, log + sleep + retry.
//   3. If gate returns {ok: false} and we are out of attempts
//      (or the stage is not retryable), call onGateFailure
//      (log + post "failed" + set state.parseFailed) and
//      return. The executor's loop sees state.parseFailed and
//      stops.
//   4. If gate passes, call run(ctx, deps, overrides, state).
//   5. If run throws and the error is non-retryable (err.nonRetryable
//      === true), rethrow immediately. The orchestrator catch
//      in index.mjs translates it to a dashboard "failed" +
//      "failed" status PATCH + a rethrow.
//   6. If run throws and there are attempts left, log + sleep
//      + retry. The retry re-calls gate (cheap), then run.
//   7. If run throws and we are out of attempts, rethrow.
//
// The sleep is exponential: base * 2^(attempt-1), capped at
// stageBackoffMaxMs. The default sleep is setTimeout-based;
// tests inject a no-op to skip the wait.
async function withRetry(stage, ctx, deps, overrides, state) {
  const maxAttempts =
    typeof deps.stageMaxAttempts === "number"
      ? deps.stageMaxAttempts
      : DEFAULT_MAX_ATTEMPTS;
  const baseMs =
    typeof deps.stageBackoffBaseMs === "number"
      ? deps.stageBackoffBaseMs
      : DEFAULT_BACKOFF_BASE_MS;
  const capMs =
    typeof deps.stageBackoffMaxMs === "number"
      ? deps.stageBackoffMaxMs
      : DEFAULT_BACKOFF_MAX_MS;
  const sleep = deps.sleep || defaultSleep;

  const attempts = stage.retryable ? maxAttempts : 1;
  let lastGateReason = null;
  let lastRunError = null;

  for (let attempt = 1; attempt <= attempts; attempt++) {
    const pre = await stage.gate(state, ctx, deps);
    if (!pre.ok) {
      lastGateReason = pre.reason;
      if (attempt < attempts) {
        deps.log("retry", `${stage.id} gate failed, retrying`, {
          attempt,
          max: attempts,
          reason: pre.reason,
        });
        await sleep(backoff(attempt, baseMs, capMs));
        continue;
      }
      // Out of attempts (or non-retryable): soft fail.
      await onGateFailure(stage.id, pre.reason, state, deps);
      return;
    }
    try {
      await stage.run(ctx, deps, overrides, state);
      return;
    } catch (err) {
      lastRunError = err;
      if (err && err.nonRetryable === true) {
        // Re-throw immediately. The orchestrator catch in
        // index.mjs will surface it.
        throw err;
      }
      if (attempt < attempts) {
        deps.log("retry", `${stage.id} run threw, retrying`, {
          attempt,
          max: attempts,
          err: String(err?.message ?? err),
        });
        await sleep(backoff(attempt, baseMs, capMs));
        continue;
      }
      // Out of attempts: rethrow.
      throw err;
    }
  }

  // If we got here, we exhausted attempts on a gate failure
  // (the run-throw path returns via throw above). The soft
  // fail was already handled in the loop; this is just a
  // backstop for the linter.
  if (lastRunError) throw lastRunError;
  if (lastGateReason) {
    await onGateFailure(stage.id, lastGateReason, state, deps);
  }
}

function backoff(attempt, baseMs, capMs) {
  const ms = baseMs * Math.pow(2, attempt - 1);
  return Math.min(ms, capMs);
}

// onGateFailure is the executor's response to a failed gate
// after retries. It logs the failure, posts the "failed"
// status, and marks state.parseFailed so the orchestrator
// (and the cleanup stage) can short-circuit downstream work.
// The status message is the gate's reason verbatim, so a
// "summary parse failed: <X>" reason surfaces as a
// "summary parse failed: <X>" status line (matching the
// pre-QUB-90 user-visible surface).
async function onGateFailure(stageId, reason, state, deps) {
  deps.log("gate", `${stageId} gate failed`, { reason });
  await deps.postStatus("failed", reason);
  state.parseFailed = true;
  // Mirror the QUB-102 abort path: stash the reason on
  // state.failureReason so the orchestrator's parseFailed
  // branch forwards it to the dashboard. Without this, a
  // sniff / summary parse failure reaches the dashboard
  // row as just stage="failed" with no error field, even
  // though the GitHub comment timeline carries the reason.
  state.failureReason = reason;
}

// --- macro-stage gates --------------------------------------------------

async function handshakeGate(_state, _ctx, _deps) {
  return { ok: true };
}

async function fetchGate(state, _ctx, _deps) {
  if (!state.octokit) {
    return { ok: false, reason: "handshake did not populate state.octokit" };
  }
  return { ok: true };
}

async function sniffGate(_state, _ctx, _deps) {
  return { ok: true };
}

async function summaryGate(state, _ctx, _deps) {
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
  if (!state.review || !state.review.summary) {
    return { ok: false, reason: "no review summary; nothing to post" };
  }
  return { ok: true };
}

async function cleanupGate(state, _ctx, _deps) {
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
  // Mark the sub-workflow's "owner" so runSubWorkflow can
  // apply the per-macro skip list. The orchestrator (index.mjs)
  // populates state.sub.sniff with the already-passed sub-stage
  // ids when it reads the workflow state from the status
  // comment.
  state._subWorkflowOf = "sniff";
  try {
    await runSubWorkflow(REVIEW_SUB_STAGES, ctx, deps, overrides, state);
  } finally {
    delete state._subWorkflowOf;
  }
}

async function summaryStage(ctx, deps, _overrides, state) {
  await postReview(
    state.octokit,
    state.review.summary,
    ctx.reviewNumber,
    state.review.confidence,
    ctx,
    // QUB-103: forward the runner's logger so postReview's
    // "patched existing summary comment" / "created summary
    // comment" lines surface in the run logs. Without this
    // the dedup-vs-create decision is invisible to operators
    // triaging a duplicate-post incident. postInlineComments
    // (in inlinesStage below) already passes its deps for
    // the same reason; summaryStage is the missing
    // counterpart.
    { log: deps.log, errlog: deps.errlog },
  );
  deps.log("done", "summary comment posted", {
    review_number: ctx.reviewNumber,
    confidence: state.review.confidence,
  });
}

async function inlinesStage(ctx, deps, _overrides, state) {
  await postInlineComments(state.octokit, state.review.inlineComments, ctx, {
    log: deps.log,
    errlog: deps.errlog,
  });
}

async function cleanupStage(ctx, deps, overrides, state) {
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

async function classifyGate(state, _ctx, _deps) {
  // The classifier needs the openrouter key (for the real
  // LLM call when it lands) and the cloned repo (for the
  // diff context). The fetch stage has populated both.
  if (!state.openrouterApiKey) {
    return { ok: false, reason: "no openrouter api key" };
  }
  return { ok: true };
}

async function classifySubStage(ctx, deps, overrides, state) {
  // The classifier is overridable so a test can inject a
  // deterministic classification. The default is the stub
  // in lib/classify.mjs; a follow-up PR wires the real LLM
  // call (an OpenRouter SDK chat completion with a
  // classification prompt + the PR diff as context).
  const classifyFn = overrides.classify || defaultClassify;
  const classification = await classifyFn(ctx, deps);
  state.classification = classification;
  deps.log("classify", "classified PR", {
    type: classification.type,
    confidence: classification.confidence,
  });
}

async function dispatchGate(_state, _ctx, _deps) {
  // Dispatch needs the openrouter key (for the real expert
  // LLM calls) and the classification (from QUB-94). The
  // classify stage has populated both.
  return { ok: true };
}

async function dispatchSubStage(ctx, deps, overrides, state) {
  // Pick the experts for this PR type and run them in
  // parallel. Each expert is an independent LLM call (or
  // stub, in tests); the dispatcher does not serialize.
  //
  // The pickExperts + runExperts pair is overridable so a
  // test can inject a deterministic expert pool (e.g.,
  // one expert that returns a canned finding). The
  // defaults live in lib/experts.mjs.
  const pick = overrides.pickExperts || pickExperts;
  const run = overrides.runExperts || runExperts;
  const names = pick(state.classification || { type: "unknown" });
  deps.log("dispatch", "picked experts", { names });
  const findings = await run(names, ctx, deps, { classification: state.classification });
  state.findings = findings;
  deps.log("dispatch", "experts returned", {
    count: findings.length,
    names,
  });
}

async function gatherGate(_state, _ctx, _deps) {
  // Gather needs the findings from dispatch. The dispatch
  // stage has populated them.
  return { ok: true };
}

async function gatherSubStage(_ctx, deps, overrides, state) {
  // De-duplicate the findings. The default is the in-place
  // gather in lib/experts.mjs; a test can override (e.g., to
  // assert on the gather call without mutating state).
  if (typeof overrides.gather === "function") {
    state.findings = overrides.gather(state.findings || []);
  } else {
    state.findings = gather(state.findings || []);
  }
  deps.log("gather", "de-duplicated findings", {
    count: (state.findings || []).length,
  });
}

async function metaReviewGate(_state, _ctx, _deps) {
  // Meta-review runs after gather. The findings are
  // required (the meta-reviewer reads them). An empty
  // findings list is allowed — the meta-reviewer returns
  // { reDispatch: [] } and the narrate stage proceeds with
  // a placeholder review.
  return { ok: true };
}

async function metaReviewSubStage(ctx, deps, overrides, state) {
  // The meta-reviewer is overridable so a test can inject a
  // deterministic re-dispatch decision. The default is the
  // stub in lib/experts.mjs (no re-pass); a follow-up PR
  // wires the real LLM call.
  //
  // Bounded to one re-pass per run: the meta-reviewer is
  // called once and cannot re-loop. The merge replaces
  // findings from the re-dispatched experts in place;
  // other experts' findings are preserved.
  const metaReviewFn = overrides.metaReview || defaultMetaReview;
  const review = await metaReviewFn(
    state.findings || [],
    state.classification || { type: "unknown", confidence: "low" },
    ctx,
    deps,
  );
  const reDispatch = Array.isArray(review.reDispatch)
    ? review.reDispatch
    : [];
  if (reDispatch.length === 0) {
    deps.log("meta-review", "no re-pass requested", {
      findings: (state.findings || []).length,
    });
    return;
  }
  deps.log("meta-review", "re-dispatching experts", { reDispatch });
  const run = overrides.runExperts || runExperts;
  const newFindings = await run(reDispatch, ctx, deps, {
    classification: state.classification,
  });
  // Pass the reDispatch list to mergeByExpert so ALL old
  // findings for the re-dispatched experts are dropped,
  // even if the re-pass returned no new findings. Without
  // this, a re-pass that returns empty would silently keep
  // the old findings it was meant to reject.
  state.findings = mergeByExpert(
    state.findings || [],
    newFindings,
    new Set(reDispatch),
  );
  deps.log("meta-review", "re-pass merged", {
    reDispatched: reDispatch,
    total: state.findings.length,
  });
}

async function narrateGate(_state, _ctx, _deps) {
  // Narrate runs after gather. The findings are required
  // (the narrator reads them). An empty findings list is
  // allowed — the narrator returns a placeholder.
  return { ok: true };
}

async function narrateSubStage(ctx, deps, overrides, state) {
  // Narrator precedence:
  //   1. overrides.runOpenCodeSkill (the lib-split PR #71
  //      hook) — a caller that hasn't migrated to the
  //      multi-expert sub-workflow can inject a canned
  //      review here.
  //   2. overrides.narrate — a test or future caller
  //      that has a real narrator for the multi-expert
  //      sub-workflow injects the new shape.
  //   3. defaultRunOpenCodeSkill — the legacy single-LLM
  //      call. The real multi-expert narrator is a
  //      follow-up; until it lands, production reviews
  //      still go through the existing reviewer. Without
  //      this fallback, the narrate stage would post a
  //      placeholder summary in production.
  if (typeof overrides.runOpenCodeSkill === "function") {
    const skillFn = overrides.runOpenCodeSkill;
    state.review = await skillFn(state.openrouterApiKey, ctx, deps);
  } else if (typeof overrides.narrate === "function") {
    state.review = await overrides.narrate(state.findings || [], ctx, deps);
  } else {
    state.review = await defaultRunOpenCodeSkill(
      state.openrouterApiKey,
      ctx,
      deps,
    );
  }
  const telemetry = state.review.telemetry ?? null;
  deps.log("narrate", "narrator returned", {
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
