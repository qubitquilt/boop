// OpenRouter pipeline (shim).
//
// RF-001 split: this file used to own five concerns — SDK call
// wrapper, usage mapper, telemetry factory, prompt builder, and
// narrator orchestrator. They now live in `./openrouter/` as
// focused modules. This shim re-exports the public surface so
// existing call sites (`experts.ts`, `walkthrough.ts`,
// `workflow.ts`, and the tests) keep their import contract.
//
// New code should import from the focused module directly:
//   - `./openrouter/sdk.ts`         callOpenRouter, STEP_CAP
//   - `./openrouter/usage.ts`       extractUsage
//   - `./openrouter/telemetry.ts`   buildTelemetry, emptyTelemetry, stripOpenRouterPrefix
//   - `./openrouter/prompt.ts`      buildBoopPrompt
//   - `./openrouter/parser.ts`      parseReviewOutput
//   - `./openrouter/orchestrator.ts` runOpenCodeSkill
//   - `./openrouter/read.ts`        readWithRetry
//
// readWithRetry is also re-exported from this shim so
// `experts.ts` and `walkthrough.ts` can share the helper
// without a deeper rewrite (RF-013).

export {
  STEP_CAP,
  callOpenRouter,
  extractAssistantText,
} from "./openrouter/sdk.ts";
export { extractUsage } from "./openrouter/usage.ts";
export {
  buildTelemetry,
  emptyTelemetry,
  stripOpenRouterPrefix,
} from "./openrouter/telemetry.ts";
export { buildBoopPrompt } from "./openrouter/prompt.ts";
export { parseReviewOutput } from "./openrouter/parser.ts";
export { runOpenCodeSkill } from "./openrouter/orchestrator.ts";
export { readWithRetry } from "./openrouter/read.ts";
