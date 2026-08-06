// PR walkthrough.
//
// The first stage of the multi-expert sub-workflow. Generates
// a human-readable walkthrough of the PR — what it does, why,
// and the surface area it touches — and hands it to the expert
// dispatch. The walkthrough is the single shared context all
// experts see; the per-expert lens file is the lens-specific
// checklist the expert walks the walkthrough through.
//
// The walkthrough runs as its own LLM call. The prompt is a
// single user message: the diff range + a tight walkthrough
// request. The response is plain Markdown the runner reads
// verbatim into the expert prompts.
//
// Output shape:
//   { walkthrough: string, telemetry: TelemetryShape }
// `walkthrough` is the LLM's response text. `telemetry` mirrors
// the OpenRouter SDK usage block; the dashboard data layer
// records it under the run row.
//
// The walkthrough is bounded. The prompt asks for ~10–20
// sentences. The output is then capped at MAX_WALKTHROUGH_CHARS
// as a defense against an LLM that runs long; the LLM is asked
// to be terse, not the runner.

import { buildTelemetry } from "./openrouter.mjs";

const MAX_WALKTHROUGH_CHARS = 8000;

// buildWalkthroughPrompt is the user message the walkthrough
// LLM sees. The diff range comes from ctx (validated upstream);
// the request is terse on purpose. The walkthrough is for the
// expert sub-agents, not for the PR author — the experts need
// a sharp summary of the change so each lens can apply its
// checklist without re-reading the full diff.
export function buildWalkthroughPrompt(ctx, deps) {
  // Match the ctx shape that loadConfig + workflow.mjs
  // produce: prBaseRef + prHeadSha for first reviews;
  // previousHeadSha + prHeadSha for re-reviews. The diff
  // range is supplied pre-computed by workflow.mjs; the
  // fallback here matches the same shape.
  const {
    diffRange,
    prBaseRef,
    prHeadSha,
    isReReview,
    previousHeadSha,
    paths,
  } = ctx;
  const range = diffRange || (isReReview
    ? `${previousHeadSha}...${prHeadSha}`
    : `${prBaseRef}...${prHeadSha}`);
  return [
    "# Task",
    "",
    "Read the diff at the range below and produce a short walkthrough.",
    "The walkthrough is for a code-review sub-agent that will apply",
    "one of eight lens checklists against the change. The sub-agent",
    "does not need to be told what the PR does; it needs to be told",
    "what changed, where, and why — in human terms, not in commit-log",
    "terms. Aim for 10–20 sentences. Use a flat bullet list, not",
    "narrative prose; the sub-agent scans it.",
    "",
    "# What to include",
    "",
    "- One sentence for the PR's overall purpose (the why).",
    "- One bullet per logical change group, in the order they",
    "  appear in the diff. Group by intent, not by file: every",
    "  PR that touches a feature touches 3–5 files, and most",
    "  of those are coupled.",
    "- For each change group, name the files + the function or",
    "  class that changed. The sub-agent will look up the line",
    "  numbers in the diff; it does not need them here.",
    "- Note any new public surface (new exports, new env vars,",
    "  new HTTP routes, new DB tables). The sub-agent treats",
    "  these as high-signal.",
    "- Note any test changes. If tests changed shape (new",
    "  fixture, new assertion, new mock), say so. The test",
    "  lens will look up the details.",
    "- Note any doc or config change. The sub-agent does not",
    "  need the doc content, just that it changed.",
    "",
    "# What to skip",
    "",
    "- Code-level detail (line numbers, variable names, type",
    "  signatures). The sub-agent reads the diff for that.",
    "- Restating the commit message. The sub-agent already",
    "  has the diff range and the head SHA.",
    "- Review-style observations. The walkthrough describes",
    "  what the PR does. The lens produces the review.",
    "",
    "# Diff range",
    "",
    "```",
    `range: ${range}`,
    `working_directory: ${paths?.repoDir || "/work/repo"}`,
    "```",
    "",
    "Read the diff. Produce the walkthrough. Do not start with a",
    "preamble.",
  ].join("\n");
}

// generateWalkthrough calls the OpenRouter SDK with the
// walkthrough prompt and returns the walkthrough text plus
// telemetry. The runner pipes the walkthrough text into every
// expert prompt; the telemetry rolls into the run row.
//
// Telemetry is on a different model call than the expert
// calls; the runner's `state.review.telemetry` is the merged
// shape across all calls (the dashboard sums them on display).
export async function generateWalkthrough(ctx, deps) {
  const prompt = buildWalkthroughPrompt(ctx, deps);
  // The walkthrough call uses a tighter timeout than the
  // main SDK call — the walkthrough is a small structured
  // request, not a full review. 60s is enough for the
  // current model family and gives the retry machinery
  // room to re-attempt on a slow call without blowing the
  // Job's 30-min ceiling.
  const WALKTHROUGH_TIMEOUT_MS = 60_000;
  // The walkthrough uses deps.callOpenRouter when present
  // (tests inject a fake; production calls the real one
  // via the runner's deps). When deps.callOpenRouter is
  // missing (early-stage wiring), fall back to the
  // imported function so a direct call to
  // generateWalkthrough(ctx, deps) without a runner-wired
  // deps still works.
  const callOpenRouter = deps.callOpenRouter || (await import("./openrouter.mjs")).callOpenRouter;
  let callResult;
  try {
    callResult = await callOpenRouter(prompt, {
      ...deps,
      // Trim the model override: the walkthrough is small
      // and a smaller model handles it fine. Operators
      // can still override via deps.openrouterModel if
      // they need a specific model for the walkthrough.
      model: deps.walkthroughModel || deps.model,
      timeoutMs: WALKTHROUGH_TIMEOUT_MS,
      // The walkthrough call does not need the lenses in
      // context; buildBoopPrompt is for the full review.
      // The walkthrough is a standalone user message.
    });
  } catch (err) {
    // The walkthrough is not on the critical path: a
    // failure here falls back to a placeholder, the
    // experts still see the diff directly, and the run
    // produces a review. Better to log and continue than
    // to abort the whole run.
    deps.log?.("walkthrough", "llm call failed; using placeholder", {
      err: String(err?.message ?? err),
    });
    return {
      walkthrough: PLACEHOLDER_WALKTHROUGH,
      telemetry: buildTelemetry(null, err),
    };
  }
  const text = (callResult?.text || "").trim();
  const walkthrough = text.slice(0, MAX_WALKTHROUGH_CHARS);
  if (!walkthrough) {
    deps.log?.("walkthrough", "empty response; using placeholder");
    return {
      walkthrough: PLACEHOLDER_WALKTHROUGH,
      telemetry: buildTelemetry(callResult),
    };
  }
  deps.log?.("walkthrough", "generated", {
    chars: walkthrough.length,
    truncated: text.length > MAX_WALKTHROUGH_CHARS,
  });
  return {
    walkthrough,
    telemetry: buildTelemetry(callResult),
  };
}

// PLACEHOLDER_WALKTHROUGH is the fallback when the LLM call
// fails or returns empty. The experts see this and the diff;
// they do not have a curated summary, but they have the
// ground truth. The placeholder is intentionally short so it
// cannot masquerade as a real walkthrough in the run logs.
const PLACEHOLDER_WALKTHROUGH =
  "(walkthrough unavailable — experts are reading the diff directly)";
