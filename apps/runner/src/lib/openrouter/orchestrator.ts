// Narrator orchestrator.
//
// runOpenCodeSkill is the orchestrator over `prompt.ts
// buildBoopPrompt` + `sdk.ts callOpenRouter`. Returns
// { summary, inlineComments, confidence } plus a telemetry
// object. Called by the narrate sub-stage in `../workflow.ts`;
// tests inject a stub via overrides.runOpenCodeSkill.
//
// The function is named runOpenCodeSkill for historical reasons —
// the lib-split refactor in PR #71 exported it under that name,
// the QUB-89 sub-workflow refactor kept the name, and the SDK
// cutover did not rename it. The name persists for the import
// contract; the implementation is purely SDK now.

import { OPENCODE_TIMEOUT_MS } from "../config.ts";
import { shortSha } from "../security.ts";
import { lintReview, summarize } from "../ste-lint.ts";
import { buildAgentTools } from "../tools.ts";
import { callOpenRouter } from "./sdk.ts";
import { buildBoopPrompt } from "./prompt.ts";
import { parseReviewOutput } from "./parser.ts";
import { buildTelemetry } from "./telemetry.ts";
import { stripOpenRouterPrefix } from "./telemetry.ts";
import type { Ctx, Deps, Review } from "../../types.ts";

export async function runOpenCodeSkill(
  openrouterApiKey: string,
  ctx: Ctx,
  deps: Deps,
): Promise<Review> {
  const prompt = ctx.skipSkill
    ? `Reply with one sentence: confirm you can see the repo at ${deps.paths.repoDir} on head ${shortSha(ctx.prHeadSha)}.`
    : await buildBoopPrompt(ctx, deps);

  const model = stripOpenRouterPrefix(ctx.openrouterModel);
  if (!model) {
    throw new Error(
      "openrouter SDK path: OPENROUTER_MODEL is unset or empty",
    );
  }

  const tools = buildAgentTools(ctx, deps);

  deps.log("opencode", "starting", {
    dir: deps.paths.repoDir,
    model,
    mode: ctx.skipSkill ? "minimal" : "full",
    path: "openrouter-agent",
    toolCount: Array.isArray(tools) ? tools.length : 0,
  });
  await deps.postStatus("review");

  const callFn = deps.callOpenRouter || callOpenRouter;
  let callResult;
  let killed = false;
  let timeoutMs = 0;
  const startMs = Date.now();
  try {
    callResult = await callFn(prompt, {
      ...deps,
      model,
      tools,
    });
  } catch (err) {
    const elapsed = Date.now() - startMs;
    const isAbort = err instanceof Error && err.name === "AbortError";
    if (isAbort) {
      killed = true;
      timeoutMs = OPENCODE_TIMEOUT_MS;
    }
    deps.errlog("opencode", "sdk call failed", {
      killed,
      timeoutMs,
      mode: "openrouter-agent",
      error: String(err instanceof Error ? err.message : err),
      errorName: err instanceof Error ? err.name : undefined,
      elapsedMs: elapsed,
    });
    if (killed) {
      throw new Error(`openrouter run exceeded ${OPENCODE_TIMEOUT_MS / 60000}-min timeout`);
    }
    const review = parseReviewOutput("");
    review.parseError = review.parseError || "sdk call failed";
    return { ...review, telemetry: buildTelemetry(null, err) };
  }

  const review = parseReviewOutput(callResult.text);
  const telemetry = buildTelemetry(callResult);

  if (review && review.summary) {
    const reports = lintReview(review);
    const flat = summarize(reports);
    if (flat.length > 0) {
      deps.log?.("ste-lint", "drift", {
        count: flat.length,
        sample: flat.slice(0, 5),
      });
    }
  }

  deps.log("opencode", "exit", {
    killed: false,
    timeoutMs: 0,
    mode: "openrouter-agent",
    model: callResult.model,
    stdoutBytes: callResult.text.length,
    tokens_in: telemetry.inputTokens,
    tokens_out: telemetry.outputTokens,
    cost_usd: telemetry.costUsd,
    step_count: telemetry.stepCount,
  });

  if (review.parseError) {
    deps.log("review", "summary_parse_failed", {
      reason: review.parseError,
      stdoutBytes: callResult.text.length,
      preview: callResult.text.slice(0, 200),
    });
  }

  return { ...review, telemetry };
}
