// PR classifier.
//
// QUB-94: identify the PR type so the dispatch sub-stage
// (QUB-95) can pick the right experts. The classifier is
// the first sub-stage inside the `sniff` macro-stage's
// sub-workflow. It runs before the LLM-powered dispatch /
// gather / narrate pipeline.
//
// Today the classifier is a stub that returns a placeholder
// type + low confidence. A follow-up PR wires the real
// LLM call: a small OpenRouter SDK chat completion with a
// classification prompt (PR title + body + diff stats → PR
// type). The stub is enough for QUB-94's test surface; the
// override hook lets a test inject a deterministic
// classification.
//
// The classifier is a pure function over (ctx, deps). It
// does not depend on workflow.ts or any runner state —
// it's a leaf module that the workflow.ts `classify`
// sub-stage calls via the overrides hook. This keeps the
// classifier testable in isolation and makes the real-LLM
// upgrade a single-file change.

import type { Ctx, Deps, Classification } from "../types.ts";

// The PR type vocabulary. The dispatch sub-stage (QUB-95)
// maps a type to a set of experts. Adding a new type means
// updating both this list and the dispatcher's mapping.
export const PR_TYPES = [
  "feature",
  "bug-fix",
  "refactor",
  "docs",
  "test-only",
  "infra",
  "unknown",
] as const;

export type PrType = (typeof PR_TYPES)[number];

// defaultClassify is the stub. It returns a fixed
// "unknown" + "low" classification. The orchestrator can
// run with this stub (the dispatch sub-stage falls back to
// a default expert set when the type is "unknown") and the
// review still posts a real summary.
export async function defaultClassify(
  _ctx: Ctx,
  _deps: Deps,
): Promise<Classification> {
  return { type: "unknown", confidence: "low" };
}

// normalizeClassification coerces an arbitrary string to a
// known PR type. Used by the real-LLM classifier path
// (when it lands) so a model that returns "feature work"
// or "Bug fix" still maps to a known bucket.
//
// The check order matters: the more specific buckets run
// first so a fuzzy string like "add tests" lands in
// "test-only" rather than "feature" (the keyword "add"
// appears in both, but "test" is the disambiguator).
export function normalizeClassification(value: unknown): PrType {
  if (typeof value !== "string") return "unknown";
  const v = value.trim().toLowerCase();
  if ((PR_TYPES as readonly string[]).includes(v)) return v as PrType;
  if (v.includes("test")) return "test-only";
  if (v.includes("bug") || v.includes("fix")) return "bug-fix";
  if (v.includes("doc") || v.includes("readme")) return "docs";
  if (v.includes("infra") || v.includes("ci") || v.includes("build"))
    return "infra";
  if (v.includes("refactor") || v.includes("clean")) return "refactor";
  if (v.includes("feature") || v.includes("add") || v.includes("new"))
    return "feature";
  return "unknown";
}
