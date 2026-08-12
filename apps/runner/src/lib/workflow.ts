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
import { defaultClassify } from "./classify.ts";
import {
  defaultMetaReview,
  gather,
  mergeByExpert,
  pickExperts,
  placeholderNarrate,
  runExperts,
} from "./experts.ts";
import { cloneRepo } from "./git.ts";
import { runOpenCodeSkill as defaultRunOpenCodeSkill } from "./openrouter.ts";
import {
  cleanupPriorReview as defaultCleanupPriorReview,
  makeOctokit,
  mintInstallationToken,
  postReview,
  postInlineComments,
} from "./github.ts";
import { readSecretFile } from "./security.ts";
import {
  generateWalkthrough as defaultGenerateWalkthrough,
} from "./walkthrough.ts";
import { postStage as defaultPostStage } from "./dashboard.ts";
import type {
  Ctx,
  Deps,
  GateResult,
  Overrides,
  State,
  Stage,
} from "../types.ts";

export const DEFAULT_MAX_ATTEMPTS = 3;
export const DEFAULT_BACKOFF_BASE_MS = 1000;
export const DEFAULT_BACKOFF_MAX_MS = 30000;

type MacroStageId =
  | "handshake"
  | "fetch"
  | "sniff"
  | "summary"
  | "inlines"
  | "cleanup";

type SubStageId =
  | "walkthrough"
  | "classify"
  | "dispatch"
  | "gather"
  | "meta-review"
  | "narrate";

export const STAGES: readonly Stage<MacroStageId>[] = [
  {
    id: "handshake",
    statusStage: "auth",
    description:
      "Read the GitHub App private key + OpenRouter API key, mint an installation token, build the Octokit instance.",
    input: "ctx, mounted secret files",
    output:
      "state.installationToken, state.openrouterApiKey, state.octokit",
    idempotent: false,
    retryable: false,
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
    retryable: true,
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
    retryable: true,
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
    retryable: false,
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
    retryable: true,
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
] as const satisfies readonly Stage[];

export const REVIEW_SUB_STAGES: readonly Stage<SubStageId>[] = [
  {
    id: "walkthrough",
    statusStage: null,
    description:
      "Generate a human-readable walkthrough of the PR (what it does, in 10-20 sentences). Every expert consumes the walkthrough as shared context. Failure here is non-fatal: experts fall back to reading the diff directly.",
    input: "ctx (diff range, paths, head SHA), state.openrouterApiKey",
    output: "state.walkthrough = string; state.walkthroughTelemetry = telemetry",
    idempotent: false,
    retryable: true,
    gate: walkthroughGate,
    run: walkthroughSubStage,
  },
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
      "Pick the experts for this PR type and run them in parallel. Each expert is an independent OpenRouter SDK call with the lens file as system prompt and the walkthrough + diff as user message. A single expert failure rejects the whole dispatch; the workflow retry machinery re-attempts.",
    input: "state.walkthrough, state.classification",
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
] as const satisfies readonly Stage[];

export function statusStageFor(id: string): string | null {
  const stage = STAGES.find((s) => s.id === id);
  return stage ? stage.statusStage : null;
}

type WalkStagesConfig = {
  skipList?: () => Promise<string[]>;
  onSkip?: (
    stage: Stage,
    passed: string[],
  ) => Promise<{ aborted: boolean; reason?: string }> | { aborted: boolean; reason?: string };
  onStart?: (stage: Stage) => void | Promise<void>;
  onEnd?: (stage: Stage) => void | Promise<void>;
  onPass?: (stage: Stage) => void | Promise<void>;
};

type WalkResult = { aborted: true; reason?: string } | { parseFailed: true } | { ok: true };

async function walkStages(
  stages: readonly Stage[],
  ctx: Ctx,
  deps: Deps,
  overrides: Overrides,
  state: State,
  config: WalkStagesConfig = {},
): Promise<WalkResult> {
  const {
    skipList = async () => [],
    onSkip = () => ({ aborted: false }),
    onStart = () => {},
    onEnd = async () => {},
    onPass = () => {},
  } = config;
  for (const stage of stages) {
    const passed = await skipList();
    if (passed.includes(stage.id)) {
      const result = await onSkip(stage, passed);
      if (result && result.aborted) {
        return { aborted: true, reason: result.reason };
      }
      continue;
    }
    await onStart(stage);
    try {
      await withRetry(stage, ctx, deps, overrides, state);
    } finally {
      await onEnd(stage);
    }
    if (state.parseFailed) return { parseFailed: true };
    await onPass(stage);
  }
  return { ok: true };
}

export async function runStages(
  ctx: Ctx,
  deps: Deps,
  overrides: Overrides,
  state: State,
  options: { onStagePassed?: (stageId: string, state: State) => Promise<void> | void } = {},
): Promise<State> {
  const onStagePassed = options.onStagePassed || (() => {});
  const postStage = (deps.postStage as typeof defaultPostStage | undefined) || defaultPostStage;
  await walkStages(STAGES, ctx, deps, overrides, state, {
    skipList: async () => state.passed || [],
    onSkip: async (stage, passed) => {
      const reason = `another pod already passed [${passed.join(", ")}]; refusing to duplicate the review`;
      deps.errlog("abort", reason, {
        stage: stage.id,
        passed,
      });
      state.failureReason = reason;
      state.parseFailed = true;
      await deps.postStatus("failed", reason);
      return { aborted: true, reason };
    },
    onStart: (stage) => {
      // Fire-and-forget: the stage-start POST is telemetry, not
      // a correctness gate. Awaiting it would put a 5s dashboard
      // timeout * N stages on the critical path of every review;
      // dropping the promise keeps the waterfall non-blocking. The
      // dashboard helper absorbs its own errors (O2 on PR #198).
      void postStage(stage.id, ctx, deps);
    },
    onEnd: async (stage) => {
      await postStage(stage.id, ctx, deps, { ended: true });
    },
    onPass: async (stage) => {
      state.passed = state.passed || [];
      if (!state.passed.includes(stage.id)) state.passed.push(stage.id);
      await onStagePassed(stage.id, state);
    },
  });
  return state;
}

export async function runSubWorkflow(
  stages: readonly Stage[],
  ctx: Ctx,
  deps: Deps,
  overrides: Overrides,
  state: State,
): Promise<void> {
  const macroId = state._subWorkflowOf;
  await walkStages(stages, ctx, deps, overrides, state, {
    skipList: async () => {
      if (!macroId || !state.sub || !Array.isArray(state.sub[macroId])) {
        return [];
      }
      return state.sub[macroId] as string[];
    },
    onSkip: (stage, sub) => {
      deps.log("resume", `sub-stage ${stage.id} already passed; skipping`, {
        sub,
      });
      return { aborted: false };
    },
    onPass: (stage) => {
      if (macroId) {
        state.sub = state.sub || {};
        state.sub[macroId] = state.sub[macroId] || [];
        if (!(state.sub[macroId] as string[]).includes(stage.id)) {
          (state.sub[macroId] as string[]).push(stage.id);
        }
      }
    },
  });
}

async function withRetry(
  stage: Stage,
  ctx: Ctx,
  deps: Deps,
  overrides: Overrides,
  state: State,
): Promise<void> {
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
  let lastGateReason: string | null = null;
  let lastRunError: unknown = null;

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
      await onGateFailure(stage.id, pre.reason, state, deps);
      return;
    }
    try {
      await stage.run(ctx, deps, overrides, state);
      return;
    } catch (err) {
      lastRunError = err;
      const nonRetryable = err && typeof err === "object" && (err as { nonRetryable?: unknown }).nonRetryable === true;
      if (nonRetryable) {
        throw err;
      }
      if (attempt < attempts) {
        deps.log("retry", `${stage.id} run threw, retrying`, {
          attempt,
          max: attempts,
          err: String(err instanceof Error ? err.message : err),
        });
        await sleep(backoff(attempt, baseMs, capMs));
        continue;
      }
      throw err;
    }
  }

  if (lastRunError) throw lastRunError;
  if (lastGateReason) {
    await onGateFailure(stage.id, lastGateReason, state, deps);
  }
}

function backoff(attempt: number, baseMs: number, capMs: number): number {
  const ms = baseMs * Math.pow(2, attempt - 1);
  return Math.min(ms, capMs);
}

async function onGateFailure(stageId: string, reason: string, state: State, deps: Deps): Promise<void> {
  deps.log("gate", `${stageId} gate failed`, { reason });
  await deps.postStatus("failed", reason);
  state.parseFailed = true;
  state.failureReason = reason;
}

// --- macro-stage gates --------------------------------------------------

async function handshakeGate(_state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  return { ok: true };
}

async function fetchGate(state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  if (!state.octokit) {
    return { ok: false, reason: "handshake did not populate state.octokit" };
  }
  return { ok: true };
}

async function sniffGate(_state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  return { ok: true };
}

async function summaryGate(state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  if (!state.review) {
    return { ok: false, reason: "no review produced" };
  }
  if (!state.review.summary) {
    const reason = state.review.parseError || "summary empty";
    return { ok: false, reason: `summary parse failed: ${reason}` };
  }
  return { ok: true };
}

async function inlinesGate(state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  if (!state.review || !state.review.summary) {
    return { ok: false, reason: "no review summary; nothing to post" };
  }
  return { ok: true };
}

async function cleanupGate(state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  if (!state.review || !state.review.summary) {
    return { ok: false, reason: "no review landed; nothing to clean up" };
  }
  return { ok: true };
}

// --- macro-stage functions ----------------------------------------------

async function handshakeStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
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
  deps.env.OPENROUTER_API_KEY = OPENROUTER_API_KEY;
  state.octokit = overrides.makeOctokit
    ? overrides.makeOctokit(installationToken)
    : makeOctokit(installationToken);
  if (typeof deps.setOctokit === "function") {
    deps.setOctokit(state.octokit);
  }
  deps.log("auth", "minted installation token");
  await deps.postStatus("auth");
}

async function fetchStage(ctx: Ctx, deps: Deps, _overrides: Overrides, state: State): Promise<void> {
  await deps.cloneRepo(state.installationToken as string, ctx, deps);
  deps.log("clone", "repo cloned", {
    dir: deps.paths.repoDir,
    sha: ctx.prHeadSha,
  });
}

async function sniffStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  state._subWorkflowOf = "sniff";
  try {
    await runSubWorkflow(REVIEW_SUB_STAGES, ctx, deps, overrides, state);
  } finally {
    delete state._subWorkflowOf;
  }
}

async function summaryStage(ctx: Ctx, deps: Deps, _overrides: Overrides, state: State): Promise<void> {
  if (!state.review || !state.octokit) {
    throw new Error("summaryStage: missing review or octokit (gate should have caught this)");
  }
  await postReview(
    state.octokit,
    state.review.summary,
    ctx.reviewNumber,
    state.review.confidence,
    ctx,
    { log: deps.log, errlog: deps.errlog } as { log: Deps["log"]; errlog: Deps["errlog"] },
  );
  deps.log("done", "summary comment posted", {
    review_number: ctx.reviewNumber,
    confidence: state.review.confidence,
  });
}

async function inlinesStage(ctx: Ctx, deps: Deps, _overrides: Overrides, state: State): Promise<void> {
  if (!state.review || !state.octokit) {
    throw new Error("inlinesStage: missing review or octokit (gate should have caught this)");
  }
  await postInlineComments(state.octokit, state.review.inlineComments, ctx, {
    log: deps.log,
    errlog: deps.errlog,
  });
}

async function cleanupStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  if (ctx.reviewNumber <= 1 || !ctx.botLogin) {
    state.cleanup = null;
    return;
  }
  if (!state.installationToken) {
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
      err: String(err instanceof Error ? err.message : err),
    });
    state.cleanup = { resolved: 0, minimized: 0, errors: 1 };
  }
}

// --- sub-stage gate + function -----------------------------------------

async function walkthroughGate(state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  if (!state.openrouterApiKey) {
    return { ok: false, reason: "no openrouter api key" };
  }
  return { ok: true };
}

async function walkthroughSubStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  const gen = overrides.generateWalkthrough || defaultGenerateWalkthrough;
  const { walkthrough, telemetry } = await gen(ctx, deps);
  state.walkthrough = walkthrough;
  state.walkthroughTelemetry = telemetry;
  deps.log("walkthrough", "walkthrough generated", {
    chars: (walkthrough || "").length,
    isPlaceholder: walkthrough.startsWith("(walkthrough unavailable"),
  });
}

async function classifyGate(state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  if (!state.openrouterApiKey) {
    return { ok: false, reason: "no openrouter api key" };
  }
  return { ok: true };
}

async function classifySubStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  const classifyFn = overrides.classify || defaultClassify;
  const classification = await classifyFn(ctx, deps);
  state.classification = classification;
  deps.log("classify", "classified PR", {
    type: classification.type,
    confidence: classification.confidence,
  });
}

async function dispatchGate(_state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  return { ok: true };
}

async function dispatchSubStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  const pick = overrides.pickExperts || pickExperts;
  const run = overrides.runExperts || runExperts;
  const names = pick(state.classification || { type: "unknown", confidence: "low" });
  deps.log("dispatch", "picked experts", { names });
  const shared = {
    classification: state.classification,
    walkthrough: state.walkthrough,
    findings: [],
  };
  const result = (await run(names, ctx, deps, shared)) as { findings?: State["findings"]; lensTelemetry?: State["lensTelemetry"] } | State["findings"] | undefined;
  const findings = Array.isArray(result) ? result : (result?.findings || []);
  const lensTelemetry = Array.isArray(result) ? [] : (result?.lensTelemetry || []);
  state.findings = findings;
  state.lensTelemetry = lensTelemetry;
  deps.log("dispatch", "experts returned", {
    count: findings.length,
    names,
    lensRows: lensTelemetry.length,
  });
}

async function gatherGate(_state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  return { ok: true };
}

async function gatherSubStage(_ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  if (typeof overrides.gather === "function") {
    state.findings = overrides.gather(state.findings || []);
  } else {
    state.findings = gather(state.findings || []);
  }
  deps.log("gather", "de-duplicated findings", {
    count: (state.findings || []).length,
  });
}

async function metaReviewGate(_state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  return { ok: true };
}

async function metaReviewSubStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  const metaReviewFn = overrides.metaReview || defaultMetaReview;
  const review = await metaReviewFn(
    state.findings || [],
    state.classification || { type: "unknown", confidence: "low" },
    ctx,
    deps,
  );
  const reDispatch = Array.isArray(review.reDispatch) ? review.reDispatch : [];
  if (reDispatch.length === 0) {
    deps.log("meta-review", "no re-pass requested", {
      findings: (state.findings || []).length,
    });
    return;
  }
  deps.log("meta-review", "re-dispatching experts", { reDispatch });
  const run = overrides.runExperts || runExperts;
  const reRun = (await run(reDispatch, ctx, deps, {
    classification: state.classification,
  })) as { findings?: State["findings"]; lensTelemetry?: State["lensTelemetry"] } | State["findings"] | undefined;
  const newFindings = Array.isArray(reRun) ? reRun : (reRun?.findings || []);
  const newLensRows = Array.isArray(reRun) ? [] : (reRun?.lensTelemetry || []);
  state.findings = mergeByExpert(
    state.findings || [],
    newFindings,
    new Set(reDispatch),
  );
  const reDispatchSet = new Set(reDispatch);
  const keptLens = (state.lensTelemetry || []).filter(
    (l) => !reDispatchSet.has(l.lens),
  );
  state.lensTelemetry = [...keptLens, ...newLensRows];
  deps.log("meta-review", "re-pass merged", {
    reDispatched: reDispatch,
    total: state.findings.length,
    lensRows: state.lensTelemetry.length,
  });
}

async function narrateGate(_state: State, _ctx: Ctx, _deps: Deps): Promise<GateResult> {
  return { ok: true };
}

async function narrateSubStage(ctx: Ctx, deps: Deps, overrides: Overrides, state: State): Promise<void> {
  const findings = state.findings || [];
  const walkthrough = state.walkthrough || "";
  const walkthroughIsPlaceholder = walkthrough.startsWith(
    "(walkthrough unavailable",
  );
  if (typeof overrides.runOpenCodeSkill === "function") {
    const skillFn = overrides.runOpenCodeSkill;
    state.review = await skillFn(state.openrouterApiKey || "", ctx, deps);
  } else if (typeof overrides.narrate === "function") {
    state.review = await overrides.narrate(findings, ctx, deps);
  } else if (findings.length === 0) {
    deps.log("narrate", "placeholder used (0 findings)", {
      walkthrough_chars: walkthrough.length,
      walkthrough_is_placeholder: walkthroughIsPlaceholder,
    });
    state.review = placeholderNarrate(
      findings,
      walkthrough,
      walkthroughIsPlaceholder,
      ctx,
      deps,
    );
  } else {
    const narrateCtx = { ...ctx, walkthrough, findings };
    state.review = await defaultRunOpenCodeSkill(
      state.openrouterApiKey || "",
      narrateCtx,
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
