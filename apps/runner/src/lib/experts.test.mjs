import { test } from "node:test";
import assert from "node:assert/strict";

import {
  pickExperts,
  EXPERT_POOL,
  runExperts,
  gather,
  defaultNarrate,
} from "./experts.mjs";

// --- pickExperts ------------------------------------------------------

test("pickExperts maps every PR type to a non-empty expert list", () => {
  // Pinned by QUB-95. The orchestrator's mapping is the
  // contract for which experts see which PR type. An
  // unmapped type would mean the dispatcher ran with no
  // experts (a silent no-op review).
  const types = [
    "feature",
    "bug-fix",
    "refactor",
    "docs",
    "test-only",
    "infra",
    "unknown",
  ];
  for (const t of types) {
    const picked = pickExperts({ type: t });
    assert.ok(Array.isArray(picked), `pickExperts(${t}) should return an array`);
    assert.ok(picked.length > 0, `pickExperts(${t}) should be non-empty`);
    for (const name of picked) {
      assert.ok(
        Object.prototype.hasOwnProperty.call(EXPERT_POOL, name),
        `pickExperts(${t}) returned unknown expert "${name}"`,
      );
    }
  }
});

test("pickExperts falls back to a default pool for an unmapped type", () => {
  // Defensive: a future PR type that doesn't get a mapping
  // should still produce a non-empty expert list (the
  // "unknown" branch).
  const picked = pickExperts({ type: "completely-new-type" });
  assert.ok(picked.length > 0);
});

test("pickExperts handles a missing or null classification", () => {
  const a = pickExperts(null);
  const b = pickExperts(undefined);
  const c = pickExperts({});
  assert.ok(a.length > 0);
  assert.ok(b.length > 0);
  assert.ok(c.length > 0);
});

// --- EXPERT_POOL -------------------------------------------------------

test("EXPERT_POOL exposes the experts pickExperts can return", () => {
  // The orchestrator's output must be resolvable. This test
  // enumerates every expert name pickExperts can ever
  // return and asserts each is in the pool.
  const allTypes = [
    "feature",
    "bug-fix",
    "refactor",
    "docs",
    "test-only",
    "infra",
    "unknown",
  ];
  const seen = new Set();
  for (const t of allTypes) {
    for (const name of pickExperts({ type: t })) seen.add(name);
  }
  for (const name of seen) {
    assert.equal(typeof EXPERT_POOL[name], "function", `EXPERT_POOL[${name}] must be a function`);
  }
});

// --- runExperts --------------------------------------------------------

test("runExperts runs the named experts in parallel", async () => {
  // The two experts run concurrently. The test records
  // the start time of each expert; both should start before
  // either finishes.
  let firstStarted = 0;
  let secondStarted = 0;
  const deps = {};
  const overrides = {
    pool: {
      "slow-1": async () => {
        firstStarted = Date.now();
        await new Promise((r) => setTimeout(r, 50));
        return { findings: [{ id: "a", expert: "slow-1", severity: "info", title: "A", body: "a" }] };
      },
      "slow-2": async () => {
        secondStarted = Date.now();
        await new Promise((r) => setTimeout(r, 50));
        return { findings: [{ id: "b", expert: "slow-2", severity: "info", title: "B", body: "b" }] };
      },
    },
  };
  // We need to override the pool the test uses. The simplest
  // path: temporarily replace EXPERT_POOL via a tiny shim.
  // Since runExperts reads from EXPERT_POOL directly, we
  // monkey-patch for the duration of the test.
  const original = { ...EXPERT_POOL };
  Object.assign(EXPERT_POOL, overrides.pool);
  try {
    const t0 = Date.now();
    const findings = await runExperts(["slow-1", "slow-2"], {}, deps, {});
    const elapsed = Date.now() - t0;
    assert.equal(findings.length, 2);
    // Parallel: elapsed should be ~50ms (one slow expert),
    // not ~100ms (sequential).
    assert.ok(elapsed < 90, `runExperts was sequential (elapsed=${elapsed}ms)`);
    // Both experts started before either finished.
    assert.ok(Math.abs(firstStarted - secondStarted) < 30, "experts started in parallel");
  } finally {
    for (const k of Object.keys(overrides.pool)) delete EXPERT_POOL[k];
    Object.assign(EXPERT_POOL, original);
  }
});

test("runExperts throws on an unknown expert name", async () => {
  await assert.rejects(
    () => runExperts(["does-not-exist"], {}, {}),
    /unknown expert/,
  );
});

test("runExperts concatenates findings from every expert", async () => {
  // The default experts are real LLM calls (QUB-95 +
  // multi-expert). For this test we inject canned experts
  // via deps.expertOverrides so the assertion is
  // deterministic. The default-expert behavior is covered
  // in lib/walkthrough.test.mjs and the real-expert
  // prompt-shape tests below.
  const deps = {
    expertOverrides: {
      "regression-hunter": async () => ({
        findings: [
          { id: "a", expert: "regression-hunter", severity: "info", title: "A", body: "a" },
        ],
      }),
      "test-quality": async () => ({
        findings: [
          { id: "b", expert: "test-quality", severity: "info", title: "B", body: "b" },
        ],
      }),
    },
  };
  const findings = await runExperts(
    ["regression-hunter", "test-quality"],
    {},
    deps,
  );
  assert.equal(findings.length, 2);
  const expertNames = findings.map((f) => f.expert);
  assert.ok(expertNames.includes("regression-hunter"));
  assert.ok(expertNames.includes("test-quality"));
});

// --- gather ------------------------------------------------------------

test("gather de-dupes findings by id", () => {
  const findings = [
    { id: "a", expert: "x", severity: "info", title: "A", body: "a" },
    { id: "a", expert: "y", severity: "info", title: "A2", body: "a2" },
    { id: "b", expert: "z", severity: "info", title: "B", body: "b" },
  ];
  const out = gather(findings);
  assert.equal(out.length, 2);
  assert.deepEqual(out.map((f) => f.id), ["a", "b"]);
});

test("gather drops findings with no id", () => {
  const findings = [
    { id: "a", expert: "x", severity: "info", title: "A", body: "a" },
    { expert: "y", severity: "info", title: "no-id", body: "x" },
  ];
  const out = gather(findings);
  assert.equal(out.length, 1);
  assert.equal(out[0].id, "a");
});

test("gather handles empty / null input", () => {
  assert.deepEqual(gather([]), []);
  assert.deepEqual(gather(null), []);
  assert.deepEqual(gather(undefined), []);
});

// --- defaultNarrate ----------------------------------------------------

test("defaultNarrate returns a review with summary + inlines (QUB-95 surface)", async () => {
  // The narrator's output shape is the same as
  // runOpenCodeSkill's: { summary, inlineComments,
  // confidence, telemetry }. The downstream summary +
  // inlines stages read this shape.
  const findings = [
    { id: "a", expert: "api-design", severity: "info", title: "A", body: "a" },
    {
      id: "b",
      expert: "test-quality",
      severity: "warning",
      title: "B",
      body: "b",
      path: "src/foo.ts",
      line: 10,
    },
  ];
  const review = await defaultNarrate(findings, {}, {});
  assert.equal(typeof review.summary, "string");
  assert.ok(review.summary.length > 0);
  assert.ok(Array.isArray(review.inlineComments));
  assert.equal(review.inlineComments.length, 1);
  assert.equal(review.inlineComments[0].path, "src/foo.ts");
  assert.equal(review.inlineComments[0].line, 10);
  assert.equal(review.confidence, "medium");
  assert.equal(review.telemetry, null);
});

test("defaultNarrate returns a placeholder review when no findings", async () => {
  const review = await defaultNarrate([], {}, {});
  assert.equal(typeof review.summary, "string");
  assert.ok(review.summary.length > 0);
  assert.deepEqual(review.inlineComments, []);
  assert.equal(review.confidence, "low");
});
