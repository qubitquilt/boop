// Expert registry (RF-008).
//
// Single source of truth for the expert ↔ lens mapping.
// Three maps used to live in lib/experts.ts:
//   - LENS_TO_EXPERT (lens name → expert name)
//   - EXPERT_POOL   (expert name → expert function)
//   - EXPERT_TO_LENS (expert name → lens name, derived)
//
// Renaming an expert ("error-handling" → "error-handling-expert")
// used to require touching all three. After this refactor, the
// metadata lives here as `EXPERTS` and the other two maps are
// derived from it. Renaming is one edit.
//
// EXPERT_POOL stays in lib/experts.ts because the function
// binding (`defaultExpert.bind(null, name)`) depends on
// `defaultExpert`, which is defined there. The function map
// is built from the names in EXPERTS — adding a new expert
// means adding a row here + (optionally) defining a custom
// function in the pool builder.
//
// The "review-code-quality" and "review-solid-principles"
// lenses are intentionally NOT in EXPERTS — they exist as
// files but do not have per-expert LLM calls (the single-LLM
// path uses them via the orchestrator's "## Lenses" block).
// A future PR that adds a per-expert implementation adds a
// row here with `{ lens: "review-code-quality" }` and a
// matching function in the pool builder.

export type ExpertInfo = { lens: string | null };

export const EXPERTS: Record<string, ExpertInfo> = {
  "regression-hunter": { lens: "review-deep" },
  "test-quality":      { lens: "review-test-quality" },
  "api-design":        { lens: null /* TODO: lens file */ },
  "error-handling":    { lens: "review-error-handling" },
  "design-pattern":    { lens: "review-design-pattern" },
  "readability":       { lens: "review-readability" },
};

export const LENS_TO_EXPERT: Record<string, string> = Object.fromEntries(
  Object.entries(EXPERTS)
    .filter(([, info]) => info.lens)
    .map(([name, info]) => [info.lens as string, name]),
);

export const EXPERT_TO_LENS: Record<string, string> = Object.fromEntries(
  Object.entries(EXPERTS)
    .filter(([, info]) => info.lens)
    .map(([name, info]) => [name, info.lens as string]),
);
