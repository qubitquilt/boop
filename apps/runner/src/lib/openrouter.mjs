// OpenRouter pipeline (shim).
//
// RF-001 split: this file used to own five concerns — SDK call
// wrapper, usage mapper, telemetry factory, prompt builder, and
// narrator orchestrator. They now live in `./openrouter/` as
// focused modules. This shim re-exports the public surface so
// existing call sites (`experts.mjs`, `walkthrough.mjs`,
// `workflow.mjs`, and the tests) keep their import contract.
//
// New code should import from the focused module directly:
//   - `./openrouter/sdk.mjs`         callOpenRouter, STEP_CAP
//   - `./openrouter/usage.mjs`       extractUsage
//   - `./openrouter/telemetry.mjs`   buildTelemetry, emptyTelemetry, stripOpenRouterPrefix
//   - `./openrouter/prompt.mjs`      buildBoopPrompt
//   - `./openrouter/parser.mjs`      parseReviewOutput
//   - `./openrouter/orchestrator.mjs` runOpenCodeSkill
//   - `./openrouter/read.mjs`        readWithRetry
//
// readWithRetry is also re-exported from this shim so
// `experts.mjs` and `walkthrough.mjs` can share the helper
// without a deeper rewrite (RF-013).

export {
  STEP_CAP,
  callOpenRouter,
  extractAssistantText,
} from "./openrouter/sdk.mjs";
export { extractUsage } from "./openrouter/usage.mjs";
export {
  buildTelemetry,
  emptyTelemetry,
  stripOpenRouterPrefix,
} from "./openrouter/telemetry.mjs";
export { buildBoopPrompt } from "./openrouter/prompt.mjs";
export { parseReviewOutput } from "./openrouter/parser.mjs";
export { runOpenCodeSkill } from "./openrouter/orchestrator.mjs";
export { readWithRetry } from "./openrouter/read.mjs";
