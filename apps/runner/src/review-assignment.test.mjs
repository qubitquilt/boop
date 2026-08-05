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
// This test pins the contract: the review-skill call must assign
// to a declared binding (a `let review;` followed by `review = ...`,
// or a `state.review = ...` where `state` is declared in the same
// file). Both shapes are valid; the bug the test guards against is
// the third shape — an undeclared-variable assignment — which
// strict-mode ES modules reject at runtime.
//
// After the lib-split refactor in PR #71 the call became
// `await skillFn(...)` (with skillFn either the override or
// `runOpenCodeSkill`). After the QUB-89 refactor the call moved
// into `lib/workflow.mjs` and the assignment is `state.review = ...`
// against a `const state = {}` declared in the same function. The
// test reads both files so a future refactor that splits the call
// again does not silently re-introduce the bug. The check is
// source-text rather than runtime because the surrounding `run()`
// does too much (mints tokens, clones the repo, posts status,
// drives the SDK chat completion) to exercise cheaply in a unit
// test.

const here = path.dirname(fileURLToPath(import.meta.url));
const indexSrc = readFileSync(path.join(here, "index.mjs"), "utf8");
const workflowSrc = readFileSync(path.join(here, "lib", "workflow.mjs"), "utf8");

test("review-skill call assigns to a declared binding", () => {
  // Find the file that has the review-skill call.
  const callIdxByFile = [
    ["index.mjs", indexSrc],
    ["lib/workflow.mjs", workflowSrc],
  ].map(([label, src]) => [
    label,
    src,
    Math.max(
      src.indexOf("await runOpenCodeSkill("),
      src.indexOf("await skillFn("),
    ),
  ]);
  const found = callIdxByFile.find(([, , idx]) => idx >= 0);
  assert.ok(
    found,
    "no `await runOpenCodeSkill(` or `await skillFn(` call found in index.mjs or lib/workflow.mjs",
  );
  const [label, src] = found;

  // The two safe shapes. The unsafe shape (bare `review = await ...`
  // with no preceding `let review;` in scope) is what we guard
  // against. state.review is a property of a const-declared state
  // object, which strict-mode ES modules accept.
  const safeDeclMatch = src.match(/\blet\s+review\s*;/);
  if (safeDeclMatch) {
    // Old shape: let review; precedes the call.
    const callIdx = Math.max(
      src.indexOf("await runOpenCodeSkill("),
      src.indexOf("await skillFn("),
    );
    assert.ok(
      safeDeclMatch.index < callIdx,
      `expected 'let review;' (offset ${safeDeclMatch.index}) to appear before ` +
        `the review-skill call (offset ${callIdx}) in ${label}`,
    );
    return;
  }
  // New shape: state.review = ... with `state` declared in the
  // same file (either a `const state = ...` at top-level or a
  // function parameter `state`).
  const hasStateDecl =
    /\b(?:const|let)\s+state\s*=/.test(src) ||
    /\bfunction\s+\w+\s*\([^)]*\bstate\b/.test(src);
  assert.ok(
    hasStateDecl,
    `review-skill call in ${label} is not assigned to a declared binding ` +
      `(no 'let review;', no 'const state = ...', and no function parameter named 'state')`,
  );
});
