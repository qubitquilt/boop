// Expert registry (RF-008).
//
// Single source of truth for the expert ↔ lens mapping.
// Three maps used to live in lib/experts.mjs:
//   - LENS_TO_EXPERT (lens name → expert name)
//   - EXPERT_POOL   (expert name → expert function)
//   - EXPERT_TO_LENS (expert name → lens name, derived)
//
// Renaming an expert ("error-handling" → "error-handling-expert")
// used to require touching all three. After this refactor, the
// metadata lives here as `EXPERTS` and the other two maps are
// derived from it. Renaming is one edit.
//
// EXPERT_POOL stays in lib/experts.mjs because the function
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

// EXPERTS: the single source of truth.
// Each key is an expert name; the value describes the
// expert's lens file (or `null` for "no lens yet, the expert
// runs without a system prompt — fallback for stub experts").
export const EXPERTS = {
  "regression-hunter": { lens: "review-deep" },
  "test-quality":      { lens: "review-test-quality" },
  "api-design":        { lens: null /* TODO: lens file */ },
  "error-handling":    { lens: "review-error-handling" },
  "design-pattern":    { lens: "review-design-pattern" },
  "readability":       { lens: "review-readability" },
};

// LENS_TO_EXPERT: lens name → expert name. Derived: every
// expert with a non-null lens contributes a (lens, expert)
// pair. The downstream code (defaultExpert) uses this to
// resolve an expert to its lens file.
export const LENS_TO_EXPERT = Object.fromEntries(
  Object.entries(EXPERTS)
    .filter(([, info]) => info.lens)
    .map(([name, info]) => [info.lens, name]),
);

// EXPERT_TO_LENS: expert name → lens name. The reverse of
// LENS_TO_EXPERT, derived the same way. `defaultExpert`
// reads this to find the lens file for the requested
// expert; the error message references LENS_TO_EXPERT
// because the user-facing direction is "given a lens,
// which expert reads it?".
export const EXPERT_TO_LENS = Object.fromEntries(
  Object.entries(EXPERTS)
    .filter(([, info]) => info.lens)
    .map(([name, info]) => [name, info.lens]),
);
