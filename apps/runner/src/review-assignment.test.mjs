import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Regression: the 2026-07-30 security audit refactor (commit 96bb92fb)
// added an inner try/catch around the `runOpenCodeSkill` call but
// dropped the `let review;` declaration that used to sit above the
// outer try. In strict-mode ES modules the assignment then throws
// `ReferenceError: review is not defined`, the Job exits non-zero,
// K8s retries per `backoffLimit`, and the PR status timeline shows
// N copies of `auth -> review -> failed`.
//
// This test pins the contract: the `review = await runOpenCodeSkill(...)`
// assignment must live inside a try block that also declares
// `let review;`. The check is source-text rather than runtime because
// the surrounding `main()` does too much (mints tokens, clones the
// repo, posts status, drives the opencode TUI) to exercise cheaply
// in a unit test. If the structure of `main()` changes, this test
// will need to move with it — the failure is intentional.

const here = path.dirname(fileURLToPath(import.meta.url));
const indexSrc = readFileSync(path.join(here, "index.mjs"), "utf8");

test("review = await runOpenCodeSkill is preceded by a let review; declaration", () => {
  // The buggy code (commit 96bb92fb) had no `let review;` declaration
  // anywhere — assigning to an undeclared `review` throws
  // ReferenceError in strict-mode ES modules. This test asserts the
  // declaration is present and precedes the assignment. The check is
  // intentionally a flat source-text scan: lexical scope tracking
  // (matching `{` / `}` depth) is brittle and unnecessary for a
  // regression that only ever involved a missing declaration.
  const declRe = /\blet\s+review\s*;/;
  const declMatch = indexSrc.match(declRe);
  assert.ok(declMatch, "no `let review;` declaration found in index.mjs");

  const assignIdx = indexSrc.indexOf("review = await runOpenCodeSkill(");
  assert.ok(
    assignIdx >= 0,
    "no `review = await runOpenCodeSkill(...)` assignment found in index.mjs",
  );

  assert.ok(
    declMatch.index < assignIdx,
    `expected 'let review;' (offset ${declMatch.index}) to appear before ` +
      `'review = await runOpenCodeSkill(...)' (offset ${assignIdx})`,
  );
});
