import { test } from "node:test";
import assert from "node:assert/strict";

// Locks the structured output contract between the prompt and the
// runner. The opencode assistant must emit:
//
//   === SUMMARY ===
//   <markdown>
//   === INLINE COMMENTS ===
//   <path:line: body lines>
//   === CONFIDENCE ===
//   <high|medium|low>
//   === END ===
//
// and the runner parses that into { summary, inlineComments, confidence }.
// Older model output (no CONFIDENCE block) defaults to `medium` so
// missing confidence never breaks the review post path.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const indexSrc = readFileSync(path.join(here, "index.mjs"), "utf8");

function parseReviewOutput(output) {
  const match = indexSrc.match(
    /function parseReviewOutput\(output\) \{[\s\S]*?\n\}/,
  );
  assert.ok(match, "parseReviewOutput function not found in index.mjs");
  const fnSrc = match[0]
    .replace(/^function parseReviewOutput\(output\) \{/, "")
    .replace(/\n\}$/, "");
  return new Function("output", fnSrc);
}
const parser = parseReviewOutput();

test("parseReviewOutput extracts summary, inline comments, and confidence=high", () => {
  const out = parser(
    "ignored TUI transcript\n" +
      "=== SUMMARY ===\n" +
      "## TL;DR\nLooks good.\n" +
      "=== INLINE COMMENTS ===\n" +
      "src/foo.ts:42: heads up on line 42\n" +
      "src/bar.ts:7: nice\n" +
      "=== CONFIDENCE ===\n" +
      "high\n" +
      "=== END ===\n",
  );
  assert.equal(out.confidence, "high");
  assert.equal(out.summary, "## TL;DR\nLooks good.");
  assert.deepEqual(out.inlineComments, [
    { path: "src/foo.ts", line: 42, body: "heads up on line 42" },
    { path: "src/bar.ts", line: 7, body: "nice" },
  ]);
});

test("parseReviewOutput normalises confidence to medium|low|high", () => {
  for (const value of ["HIGH", "Medium", "low", " High "]) {
    const out = parser(
      "=== SUMMARY ===\nbody\n=== INLINE COMMENTS ===\n=== CONFIDENCE ===\n" +
        value +
        "\n=== END ===\n",
    );
    assert.ok(
      ["high", "medium", "low"].includes(out.confidence),
      `unexpected confidence for ${value}: ${out.confidence}`,
    );
  }
});

test("parseReviewOutput defaults confidence to medium when block is missing", () => {
  const out = parser(
    "=== SUMMARY ===\nbody\n=== INLINE COMMENTS ===\n=== END ===\n",
  );
  assert.equal(out.confidence, "medium");
});

test("parseReviewOutput defaults confidence to medium when value is unrecognised", () => {
  const out = parser(
    "=== SUMMARY ===\nbody\n=== INLINE COMMENTS ===\n=== CONFIDENCE ===\n" +
      "probably fine\n=== END ===\n",
  );
  assert.equal(out.confidence, "medium");
});

test("parseReviewOutput falls back to whole output when no structured block", () => {
  const out = parser("the model went off-script entirely");
  assert.equal(out.summary, "the model went off-script entirely");
  assert.deepEqual(out.inlineComments, []);
  assert.equal(out.confidence, "medium");
});

test("parseReviewOutput skips inline lines that do not match path:line: body", () => {
  const out = parser(
    "=== SUMMARY ===\nbody\n=== INLINE COMMENTS ===\n" +
      "not a real comment line\n" +
      "src/foo.ts:42: a real one\n" +
      "src/foo.ts:notanumber: bad line number\n" +
      "=== CONFIDENCE ===\nlow\n=== END ===\n",
  );
  assert.deepEqual(out.inlineComments, [
    { path: "src/foo.ts", line: 42, body: "a real one" },
  ]);
  assert.equal(out.confidence, "low");
});