import { test } from "node:test";
import assert from "node:assert/strict";

import {
  PR_TYPES,
  defaultClassify,
  normalizeClassification,
} from "./classify.mjs";

test("PR_TYPES is a closed vocabulary the dispatcher keys on", () => {
  // Pinned by QUB-94. The dispatch sub-stage (QUB-95) maps
  // a type to a set of experts. Adding a new type means
  // updating both PR_TYPES and the dispatcher's mapping.
  // Removing or renaming a type is a breaking change for
  // the dispatcher + every prior run that wrote the type
  // into the dashboard.
  assert.ok(Array.isArray(PR_TYPES));
  assert.ok(PR_TYPES.includes("unknown"), "PR_TYPES must include 'unknown' as the default");
  for (const t of [
    "feature",
    "bug-fix",
    "refactor",
    "docs",
    "test-only",
    "infra",
  ]) {
    assert.ok(PR_TYPES.includes(t), `PR_TYPES missing ${t}`);
  }
});

test("defaultClassify returns the placeholder classification", async () => {
  // QUB-94 ships a stub. A follow-up PR wires the real
  // LLM call; the test surface for QUB-94 is the stub +
  // the override hook.
  const result = await defaultClassify({}, {});
  assert.equal(result.type, "unknown");
  assert.equal(result.confidence, "low");
});

test("normalizeClassification maps known types verbatim", () => {
  // The real classifier (when it lands) can return any
  // string the LLM emits. The normalizer coerces it to a
  // known bucket so the dispatcher's mapping never sees a
  // surprise.
  for (const t of PR_TYPES) {
    assert.equal(normalizeClassification(t), t);
  }
  assert.equal(normalizeClassification("FEATURE"), "feature");
  assert.equal(normalizeClassification("  feature  "), "feature");
});

test("normalizeClassification maps fuzzy types to the right bucket", () => {
  assert.equal(normalizeClassification("bug fix"), "bug-fix");
  assert.equal(normalizeClassification("Bug Fix"), "bug-fix");
  assert.equal(normalizeClassification("feature work"), "feature");
  assert.equal(normalizeClassification("add new endpoint"), "feature");
  assert.equal(normalizeClassification("refactor this"), "refactor");
  assert.equal(normalizeClassification("clean up"), "refactor");
  assert.equal(normalizeClassification("update docs"), "docs");
  assert.equal(normalizeClassification("readme"), "docs");
  assert.equal(normalizeClassification("add tests"), "test-only");
  assert.equal(normalizeClassification("ci config"), "infra");
  assert.equal(normalizeClassification("build script"), "infra");
});

test("normalizeClassification falls back to 'unknown' on garbage", () => {
  assert.equal(normalizeClassification(""), "unknown");
  assert.equal(normalizeClassification("nonsense"), "unknown");
  assert.equal(normalizeClassification(null), "unknown");
  assert.equal(normalizeClassification(undefined), "unknown");
  assert.equal(normalizeClassification(42), "unknown");
});
