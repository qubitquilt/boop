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

import { buildTelemetry, stripOpenRouterPrefix } from "./openrouter.ts";
import { reviewRange } from "./security.ts";
import type { Ctx, Deps, Telemetry } from "../types.ts";

const MAX_WALKTHROUGH_CHARS = 8000;

export function buildWalkthroughPrompt(ctx: Ctx, _deps: Deps): string {
  const { diffRange, paths } = ctx;
  const range = diffRange || reviewRange(ctx) || `${ctx.prBaseRef}...${ctx.prHeadSha}`;
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

export async function generateWalkthrough(
  ctx: Ctx,
  deps: Deps,
): Promise<{ walkthrough: string; telemetry: Telemetry | null }> {
  const prompt = buildWalkthroughPrompt(ctx, deps);
  const WALKTHROUGH_TIMEOUT_MS = 60_000;
  const callOpenRouterFn = deps.callOpenRouter || ((await import("./openrouter.ts")).callOpenRouter);
  let callResult: import("../types.ts").CallResult | undefined;
  try {
    callResult = await callOpenRouterFn(prompt, {
      ...deps,
      model: deps.walkthroughModel || stripOpenRouterPrefix(ctx.openrouterModel),
      timeoutMs: WALKTHROUGH_TIMEOUT_MS,
    });
  } catch (err) {
    deps.log?.("walkthrough", "llm call failed; using placeholder", {
      err: String(err instanceof Error ? err.message : err),
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

const PLACEHOLDER_WALKTHROUGH =
  "(walkthrough unavailable — experts are reading the diff directly)";
