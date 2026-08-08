import { test } from "node:test";
import assert from "node:assert/strict";

import {
  buildWalkthroughPrompt,
  generateWalkthrough,
} from "./walkthrough.mjs";

// QUB-95 + multi-expert: the walkthrough is the first
// stage of the sub-workflow. It produces a human-readable
// summary of the PR that every expert consumes as shared
// context. These tests pin:
//
//   1. The prompt shape: terse, structured, asks for ~10-20
//      sentences, includes the diff range.
//   2. The fallback: an LLM call failure or empty response
//      produces a placeholder, not a hard error.
//   3. The success path: the LLM response is truncated to
//      MAX_WALKTHROUGH_CHARS as a defense against runaway
//      output.
//
// The walkthrough does NOT use the rtk adapter (it does
// not read files). It is a single LLM call. Tests inject
// a fake `callOpenRouter` via deps.

const baseCtx = {
  prOwner: "qubitquilt",
  prRepo: "boop",
  prNumber: 42,
  prHeadSha: "0123456789abcdef0123456789abcdef01234567",
  prBaseRef: "main",
  previousHeadSha: null,
  reviewNumber: 1,
  isReReview: false,
  diffRange: "main...0123456789abcdef0123456789abcdef01234567",
  paths: { repoDir: "/work/repo" },
  // QUB-117: the walkthrough resolves the model name from
  // ctx.openrouterModel via stripOpenRouterPrefix; the test
  // fixtures must set it so the resolved name reaches
  // callOpenRouter.
  openrouterModel: "test-model",
};

function recordingCallOpenRouter(responses) {
  const calls = [];
  const queue = [...responses];
  const fn = async (prompt, opts) => {
    calls.push({ prompt, opts });
    const next = queue.shift();
    if (!next) {
      throw new Error("no response queued");
    }
    if (next.throw) throw next.throw;
    return {
      text: next.text ?? "",
      model: next.model ?? "test-model",
      usage: next.usage ?? { prompt_tokens: 0, completion_tokens: 0, cost: 0 },
    };
  };
  return { calls, fn };
}

function makeDeps(callOpenRouter) {
  return {
    callOpenRouter,
    env: {},
    model: "test-model",
    log: () => {},
    errlog: () => {},
  };
}

test("buildWalkthroughPrompt asks for a terse walkthrough with diff range", () => {
  const prompt = buildWalkthroughPrompt(baseCtx, makeDeps(() => {}));
  // Structural assertions: the prompt has the four
  // sections the prompt template promises.
  assert.match(prompt, /# Task/);
  assert.match(prompt, /# What to include/);
  assert.match(prompt, /# What to skip/);
  assert.match(prompt, /# Diff range/);
  // The diff range comes from ctx (validated upstream by
  // loadConfig + the assertSafeRef assert). The prompt
  // echoes it back.
  assert.match(prompt, /range: main\.\.\.0123456789abcdef/);
  // The working directory is the cloned repo.
  assert.match(prompt, /working_directory: \/work\/repo/);
  // The prompt is terse on purpose: 10-20 sentences.
  assert.match(prompt, /10[\u2013-]20 sentences/);
  // The prompt is for the experts, not for the PR author.
  // The LLM must not output review-style observations.
  assert.match(prompt, /walkthrough describes[\s\S]*what the PR does/);
  // The prompt asks for bullet list, not narrative prose.
  assert.match(prompt, /bullet list/);
});

test("buildWalkthroughPrompt uses previousHeadSha on re-reviews", () => {
  const ctx = {
    ...baseCtx,
    isReReview: true,
    diffRange: null,
    previousHeadSha: "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111",
  };
  const prompt = buildWalkthroughPrompt(ctx, makeDeps(() => {}));
  // The re-review diff range is
  // previousHeadSha...prHeadSha, not base...head.
  assert.match(
    prompt,
    /range: aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111\.\.\.0123456789abcdef/,
  );
});

test("generateWalkthrough returns the LLM response when the call succeeds", async () => {
  const { calls, fn } = recordingCallOpenRouter([
    { text: "## What the PR does\n\n- bullet 1\n- bullet 2" },
  ]);
  const deps = makeDeps(fn);
  const out = await generateWalkthrough(baseCtx, deps);
  assert.equal(out.walkthrough, "## What the PR does\n\n- bullet 1\n- bullet 2");
  // The walkthrough call is a single user message; the
  // call was made exactly once.
  assert.equal(calls.length, 1);
});

test("generateWalkthrough falls back to a placeholder on LLM error", async () => {
  // A thrown LLM call must not abort the run. The
  // walkthrough is non-fatal; the experts fall back to
  // reading the diff directly.
  const { fn } = recordingCallOpenRouter([
    { throw: new Error("network blip") },
  ]);
  const deps = makeDeps(fn);
  const out = await generateWalkthrough(baseCtx, deps);
  assert.match(out.walkthrough, /walkthrough unavailable/);
  // Telemetry carries the error so the run log has a
  // breadcrumb.
  assert.equal(out.telemetry.error, "network blip");
});

test("generateWalkthrough falls back to a placeholder on empty response", async () => {
  const { fn } = recordingCallOpenRouter([{ text: "" }]);
  const deps = makeDeps(fn);
  const out = await generateWalkthrough(baseCtx, deps);
  assert.match(out.walkthrough, /walkthrough unavailable/);
  // Empty response has no error stamp.
  assert.equal(out.telemetry.error, undefined);
});

test("generateWalkthrough truncates responses longer than MAX_WALKTHROUGH_CHARS", async () => {
  // A runaway LLM emits a 50KB walkthrough. The adapter
  // caps it at MAX_WALKTHROUGH_CHARS so the per-expert
  // prompt budget is bounded. The experts still see the
  // truncated walkthrough; the diff is the ground truth.
  const big = "x".repeat(50_000);
  const { fn } = recordingCallOpenRouter([{ text: big }]);
  const deps = makeDeps(fn);
  const out = await generateWalkthrough(baseCtx, deps);
  assert.ok(out.walkthrough.length <= 8000, "walkthrough should be capped at MAX_WALKTHROUGH_CHARS");
  assert.ok(out.walkthrough.length > 0);
});

// QUB-117: the walkthrough call must pass a non-empty
// `model` to callOpenRouter. The single-LLM path resolves
// the model name from `ctx.openrouterModel` via
// stripOpenRouterPrefix; the walkthrough inherits the same
// path. The pre-fix bug used `deps.model` (never populated)
// and the SDK rejected every walkthrough call with
// `callOpenRouter: model is required`. The walkthrough
// swallows the error and returns a placeholder, so the bug
// was silent — this test pins the contract.
test("generateWalkthrough passes a non-empty model to callOpenRouter (QUB-117)", async () => {
  const { calls, fn } = recordingCallOpenRouter([
    { text: "## What the PR does\n\n- bullet 1\n- bullet 2" },
  ]);
  const deps = makeDeps(fn);
  // The resolved model name is the bare form (no
  // `openrouter/` prefix) so the SDK accepts it.
  const ctx = { ...baseCtx, openrouterModel: "minimax/minimax-m3" };
  await generateWalkthrough(ctx, deps);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].opts.model, "minimax/minimax-m3");
});

// QUB-117: a prefixed model id (the openrouter/<id> form
// opencode used internally before QUB-98) must be stripped
// before reaching callOpenRouter. The single-LLM path uses
// the same stripOpenRouterPrefix helper; the walkthrough
// must match.
test("generateWalkthrough strips the openrouter/ prefix from ctx.openrouterModel (QUB-117)", async () => {
  const { calls, fn } = recordingCallOpenRouter([
    { text: "## What the PR does\n\n- bullet 1\n- bullet 2" },
  ]);
  const deps = makeDeps(fn);
  const ctx = { ...baseCtx, openrouterModel: "openrouter/minimax/minimax-m3" };
  await generateWalkthrough(ctx, deps);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].opts.model, "minimax/minimax-m3");
});
