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
// This test pins the contract: a `let review;` declaration must
// precede the opencode-skill call. After the lib-split refactor in
// PR #71 the call is `await skillFn(...)` where skillFn is either
// the override or `runOpenCodeSkill`, so we match `await skillFn(`
// OR `await runOpenCodeSkill(` for the assignment side. The check
// is source-text rather than runtime because the surrounding
// `run()` does too much (mints tokens, clones the repo, posts
// status, drives the opencode TUI) to exercise cheaply in a unit
// test. If the structure of `run()` changes, this test will need
// to move with it — the failure is intentional.

const here = path.dirname(fileURLToPath(import.meta.url));
const indexSrc = readFileSync(path.join(here, "index.mjs"), "utf8");

test("review = await runOpenCodeSkill is preceded by a let review; declaration", () => {
  // The buggy code (commit 96bb92fb) had no `let review;` declaration
  // anywhere — assigning to an undeclared `review` throws
  // ReferenceError in strict-mode ES modules. This test asserts the
  // declaration is present and precedes the opencode-skill call.
  // The check is intentionally a flat source-text scan: lexical
  // scope tracking (matching `{` / `}` depth) is brittle and
  // unnecessary for a regression that only ever involved a missing
  // declaration.
  const declRe = /\blet\s+review\s*;/;
  const declMatch = indexSrc.match(declRe);
  assert.ok(declMatch, "no `let review;` declaration found in index.mjs");

  // Accept either the original `await runOpenCodeSkill(` call or
  // the lib-split indirection `await skillFn(` (where skillFn is
  // assigned above as `overrides.runOpenCodeSkill || runOpenCodeSkill`).
  // Both must be preceded by the `let review;` declaration.
  const callIdx = Math.max(
    indexSrc.indexOf("await runOpenCodeSkill("),
    indexSrc.indexOf("await skillFn("),
  );
  assert.ok(
    callIdx >= 0,
    "no `await runOpenCodeSkill(` or `await skillFn(` call found in index.mjs",
  );

  assert.ok(
    declMatch.index < callIdx,
    `expected 'let review;' (offset ${declMatch.index}) to appear before ` +
      `the opencode-skill call (offset ${callIdx})`,
  );
});
