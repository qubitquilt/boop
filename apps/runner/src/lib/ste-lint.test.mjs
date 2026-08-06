import { test } from "node:test";
import assert from "node:assert/strict";

import { lint, lintReview, summarize } from "./ste-lint.mjs";

// STE (Simplified Technical English) linter tests.
//
// The linter is a sanity check on the LLM's review output.
// The narrator is told to follow the rules in SKILL.md; the
// linter is the guard rail that catches drift. Tests pin
// the rule set so a future change is deliberate.

test("lint returns no violations for a clean sentence", () => {
  const r = lint("The runner parses the LLM output as a single block.");
  assert.equal(r.violations.length, 0);
});

test("lint flags contractions", () => {
  const r = lint("The runner doesn't crash. It's a small change.");
  // The matcher is greedy; both "doesn't" and "It's" should fire.
  const kinds = r.violations.map((v) => v.kind);
  assert.ok(kinds.includes("contraction"));
  // At least two contractions caught.
  const contractionViolations = r.violations.filter(
    (v) => v.kind === "contraction",
  );
  assert.ok(contractionViolations.length >= 2);
});

test("lint flags semicolons", () => {
  const r = lint("First clause; second clause.");
  const kinds = r.violations.map((v) => v.kind);
  assert.ok(kinds.includes("semicolon"));
});

test("lint flags marketing adjectives", () => {
  const r = lint("This is a seamless and robust implementation.");
  const words = r.violations
    .filter((v) => v.kind === "marketing-adjective")
    .map((v) => v.match);
  assert.ok(words.includes("seamless"));
  assert.ok(words.includes("robust"));
});

test("lint flags long sentences (instructions cap = 22 words)", () => {
  // 25-word sentence: should be flagged.
  const long = "The runner reads the last block it finds, then parses the assistant text for the structured summary inline comments confidence and end markers before it surfaces the result to the operator log line";
  const r = lint(long);
  const longSentences = r.violations.filter((v) => v.kind === "long-sentence");
  assert.ok(longSentences.length > 0, "expected long-sentence violation");
  assert.equal(longSentences[0].limit, MAX_INSTRUCTION_CAP);
});

test("lint allows short sentences under the cap", () => {
  const r = lint("The runner parses the output. It lints the prose.");
  const longSentences = r.violations.filter((v) => v.kind === "long-sentence");
  assert.equal(longSentences.length, 0);
});

test("lint skips list items in the sentence-length check", () => {
  const text = [
    "- The runner is small.",
    "- The linter runs on the summary and the inline comments before posting.",
    "- The status thread is the existing surface for pull request triggers.",
  ].join("\n");
  const r = lint(text);
  const longSentences = r.violations.filter((v) => v.kind === "long-sentence");
  assert.equal(longSentences.length, 0);
});

test("lint skips markdown headings in the sentence-length check", () => {
  const r = lint("## A heading that is also a very long sentence about the structure of the linter and its rules and its application to the LLM output");
  const longSentences = r.violations.filter((v) => v.kind === "long-sentence");
  assert.equal(longSentences.length, 0);
});

test("lintReview reports the summary + each inline comment", () => {
  const review = {
    summary: "The runner is fast. It's correct.",
    inlineComments: [
      { path: "a.ts", line: 1, body: "Don't do this." },
      { path: "b.ts", line: 2, body: "This is a seamless fix." },
    ],
  };
  const reports = lintReview(review);
  // 1 summary + 2 inlines = 3 reports.
  assert.equal(reports.length, 3);
  // The summary has a contraction.
  assert.ok(
    reports[0].violations.some((v) => v.kind === "contraction"),
    "summary should have a contraction flag",
  );
  // The first inline has a contraction.
  assert.ok(
    reports[1].violations.some((v) => v.kind === "contraction"),
    "inline 1 should have a contraction flag",
  );
  // The second inline has a marketing adjective.
  assert.ok(
    reports[2].violations.some((v) => v.kind === "marketing-adjective"),
    "inline 2 should have a marketing flag",
  );
});

test("lintReview handles empty / missing fields gracefully", () => {
  assert.equal(lintReview({}).length, 0);
  assert.equal(lintReview({ summary: "" }).length, 0);
  assert.equal(lintReview({ inlineComments: [] }).length, 0);
  assert.equal(lintReview(null).length, 0);
  assert.equal(lintReview(undefined).length, 0);
});

test("summarize flattens reports into one violation list", () => {
  const reports = [
    { surface: "summary", violations: [{ kind: "contraction" }] },
    { surface: "inline-1", violations: [{ kind: "semicolon" }] },
  ];
  const flat = summarize(reports);
  assert.equal(flat.length, 2);
  assert.equal(flat[0].surface, "summary");
  assert.equal(flat[1].surface, "inline-1");
});

const MAX_INSTRUCTION_CAP = 22;
