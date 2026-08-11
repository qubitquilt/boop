// Narrator orchestrator.
//
// runOpenCodeSkill is the orchestrator over `prompt.mjs
// buildBoopPrompt` + `sdk.mjs callOpenRouter`. Returns
// { summary, inlineComments, confidence } plus a telemetry
// object. Called by the narrate sub-stage in `../workflow.mjs`;
// tests inject a stub via overrides.runOpenCodeSkill.
//
// The function is named runOpenCodeSkill for historical reasons —
// the lib-split refactor in PR #71 exported it under that name,
// the QUB-89 sub-workflow refactor kept the name, and the SDK
// cutover did not rename it. The name persists for the import
// contract; the implementation is purely SDK now.

import { OPENCODE_TIMEOUT_MS } from "../config.mjs";
import { shortSha } from "../security.mjs";
import { lintReview, summarize } from "../ste-lint.mjs";
import { buildAgentTools } from "../tools.mjs";
import { callOpenRouter } from "./sdk.mjs";
import { buildBoopPrompt } from "./prompt.mjs";
import { parseReviewOutput } from "./parser.mjs";
import { buildTelemetry } from "./telemetry.mjs";
import { stripOpenRouterPrefix } from "./telemetry.mjs";

export async function runOpenCodeSkill(openrouterApiKey, ctx, deps) {
  const prompt = ctx.skipSkill
    ? `Reply with one sentence: confirm you can see the repo at ${deps.paths.repoDir} on head ${shortSha(ctx.prHeadSha)}.`
    : await buildBoopPrompt(ctx, deps);

  // The model name comes from the OPENROUTER_MODEL env var. The
  // QUB-94 cutover used opencode.json's `model` field as the
  // fallback; QUB-98 deleted the opencode.json ConfigMap so the env
  // override is now the only source. The "openrouter/<id>" prefix
  // opencode used internally is stripped — OpenRouter's own API
  // expects the bare `provider/model` form.
  const model = stripOpenRouterPrefix(ctx.openrouterModel);
  if (!model) {
    throw new Error(
      "openrouter SDK path: OPENROUTER_MODEL is unset or empty",
    );
  }

  // Tools: the narrate stage is one of the two call sites that
  // hands the reviewer the agent tool set (run_command, read_file,
  // git_diff). The walkthrough stays single-shot because it is a
  // small structured request; the experts (experts.mjs) opt in
  // independently. `ctx.toolsEnabled` is the orchestrator's
  // explicit signal; the default `true` keeps the narrator
  // tool-armed even if a future caller forgets to set the flag.
  // buildAgentTools centralizes the toolsEnabled + deps check,
  // returning [] when ctx.toolsEnabled === false (kill switch
  // via BOOP_TOOLS_ENABLED=0) or when deps are incomplete.
  // Resolved before the "starting" log so `toolCount` is non-null
  // on the first telemetry row.
  const tools = buildAgentTools(ctx, deps);

  deps.log("opencode", "starting", {
    dir: deps.paths.repoDir,
    model,
    mode: ctx.skipSkill ? "minimal" : "full",
    path: "openrouter-agent",
    toolCount: tools.length,
  });
  await deps.postStatus("review");

  // Test injection point: deps.callOpenRouter overrides the real
  // SDK call. Production code calls the SDK directly.
  const callFn = deps.callOpenRouter || callOpenRouter;
  let callResult;
  let killed = false;
  let timeoutMs = 0;
  const startMs = Date.now();
  try {
    callResult = await callFn(prompt, {
      ...deps,
      model,
      // RF-004: deps.env.OPENROUTER_API_KEY is set once in
      // makeDeps and updated in handshakeStage. The `...deps`
      // spread above already forwards it to callOpenRouter;
      // the legacy `env: { OPENROUTER_API_KEY: openrouterApiKey }`
      // override was the per-call snap that this refactor
      // collapsed. The first parameter is kept for backward
      // compat with the override-hook contract in workflow.mjs
      // and the test fixtures; the SDK reads deps.env, not
      // the parameter.
      tools,
    });
  } catch (err) {
    const elapsed = Date.now() - startMs;
    // The SDK uses AbortError for our timeout path (we pass
    // controller.signal into callModel). Anything else is
    // a genuine SDK failure — 4xx, 5xx, network, etc. — and
    // should surface in the error pipeline, not the info one.
    const isAbort = err?.name === "AbortError";
    if (isAbort) {
      killed = true;
      timeoutMs = OPENCODE_TIMEOUT_MS;
    }
    deps.errlog("opencode", "sdk call failed", {
      killed,
      timeoutMs,
      mode: "openrouter-agent",
      error: String(err?.message ?? err),
      errorName: err?.name,
      elapsedMs: elapsed,
    });
    if (killed) {
      throw new Error(`openrouter run exceeded ${OPENCODE_TIMEOUT_MS / 60000}-min timeout`);
    }
    const review = parseReviewOutput("");
    review.parseError = review.parseError || "sdk call failed";
    // Stamp the error on the telemetry so the dashboard can
    // distinguish a failed SDK call from a successful call that
    // happened to produce an empty summary. The QUB-105 helpers
    // surface status / content-type / body alongside the message
    // so a 4xx response is diagnosable from the dashboard row
    // without digging through pod logs.
    return { ...review, telemetry: buildTelemetry(null, err) };
  }

  const review = parseReviewOutput(callResult.text);
  const telemetry = buildTelemetry(callResult);

  // QUB-115: STE lint. The narrator is told to follow
  // the rules in SKILL.md; the linter is the guard rail
  // for the mechanical ones (contractions, semicolons,
  // marketing adjectives, sentence length). The linter
  // is best-effort: it logs warnings, it does not
  // modify the output. A failure does not block the post.
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
