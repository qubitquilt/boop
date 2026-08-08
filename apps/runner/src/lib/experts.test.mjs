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

// defaultExpert must fall back to the imported callOpenRouter
// when deps.callOpenRouter is unset. Without the fallback,
// production deps (built by index.mjs's makeDeps) trigger
// "deps.callOpenRouter is not a function" on every expert
// dispatch. The walkthrough stage has the same pattern
// (walkthrough.mjs:128); this test pins the experts half so
// a future refactor that drops the fallback breaks a test
// instead of crashing the boop reviewer.
test("defaultExpert falls back to imported callOpenRouter when deps has none", async () => {
  // No callOpenRouter in deps, no expertOverrides. The
  // fallback imports the real SDK call which fails without
  // OPENROUTER_API_KEY. The error shape we assert is "the
  // SDK was reached" — NOT "deps.callOpenRouter is not a
  // function", which is the pre-fix failure.
  const deps = {
    fs: {
      readFile: async () =>
        "# test lens\nYou are a test expert. Return JSON {findings:[]}.",
    },
    postStatus: async () => {},
    env: {},
    log: () => {},
    errlog: () => {},
  };
  // QUB-117: the dispatch resolves the model from
  // `ctx.openrouterModel`; without it, callOpenRouter
  // throws "model is required" before reaching the API-key
  // check. Provide a model so the test exercises the same
  // failure shape production sees (missing API key).
  const ctx = {
    paths: { configSrc: "/tmp" },
    openrouterModel: "test-model",
  };
  try {
    await runExperts(["regression-hunter"], ctx, deps);
    // If we got here, the SDK call unexpectedly succeeded
    // (e.g. an env-level API key was set). Skip rather than
    // fail so CI doesn't get flaky on a developer's machine.
  } catch (err) {
    const msg = String(err?.message ?? err);
    assert.ok(
      !/deps\.callOpenRouter is not a function/.test(msg),
      `expert dispatch must not crash with the pre-fix error; got: ${msg}`,
    );
    // The walkthrough equivalent throws an OPENROUTER_API_KEY
    // error in the same shape. Either that or the wrapped
    // "expert dispatch failed" message is acceptable.
    assert.ok(
      /OPENROUTER_API_KEY|expert dispatch failed|API key/i.test(msg),
      `expected an SDK-call error after fallback, got: ${msg}`,
    );
  }
});

// QUB-117: every expert dispatch must pass a non-empty
// `model` to callOpenRouter. The single-LLM path resolves the
// model name from `ctx.openrouterModel` via stripOpenRouterPrefix
// (openrouter.mjs:44); the expert dispatch inherits the same
// path. The pre-fix bug passed `deps.model` (never populated)
// and crashed every dispatch with `callOpenRouter: model is
// required`. The test pins the contract even when `deps.model`
// is unset — the production deps never set it.
test("defaultExpert passes a non-empty model to callOpenRouter (QUB-117)", async () => {
  const calls = [];
  const deps = {
    // A fake callOpenRouter that captures every invocation.
    // The fake returns a JSON string the default expert
    // parses into { findings: [] }.
    callOpenRouter: async (_prompt, opts) => {
      calls.push(opts);
      return {
        text: '{"findings": []}',
        model: opts?.model ?? "test-model",
        usage: { prompt_tokens: 0, completion_tokens: 0, cost: 0 },
      };
    },
    fs: {
      readFile: async () =>
        "# test lens\nYou are a test expert. Return JSON {findings:[]}.",
    },
    postStatus: async () => {},
    // Intentionally do NOT set `deps.model`. The pre-fix
    // bug relied on `deps.model` and crashed when it was
    // unset. The fix reads from `ctx.openrouterModel`
    // instead.
    env: {},
    log: () => {},
    errlog: () => {},
  };
  // Bare model id (no prefix). The dispatch must pass the
  // resolved name through to the SDK call unchanged.
  await runExperts(
    ["regression-hunter"],
    { paths: { configSrc: "/tmp" }, openrouterModel: "minimax/minimax-m3" },
    deps,
  );
  assert.equal(calls.length, 1, "expert dispatch must invoke callOpenRouter exactly once");
  assert.ok(calls[0], "expert dispatch must capture opts");
  assert.equal(
    calls[0].model,
    "minimax/minimax-m3",
    "expert dispatch must pass the resolved model name to callOpenRouter",
  );
  assert.ok(
    typeof calls[0].model === "string" && calls[0].model.length > 0,
    "model must be a non-empty string",
  );
});

// QUB-117: a prefixed model id (the openrouter/<id> form
// opencode used internally before QUB-98) must be stripped
// before reaching callOpenRouter. The single-LLM path uses
// the same stripOpenRouterPrefix helper; the expert path
// must match.
test("defaultExpert strips the openrouter/ prefix from ctx.openrouterModel (QUB-117)", async () => {
  const calls = [];
  const deps = {
    callOpenRouter: async (_prompt, opts) => {
      calls.push(opts);
      return {
        text: '{"findings": []}',
        model: opts?.model ?? "test-model",
        usage: { prompt_tokens: 0, completion_tokens: 0, cost: 0 },
      };
    },
    fs: {
      readFile: async () =>
        "# test lens\nYou are a test expert. Return JSON {findings:[]}.",
    },
    postStatus: async () => {},
    env: {},
    log: () => {},
    errlog: () => {},
  };
  await runExperts(
    ["regression-hunter"],
    { paths: { configSrc: "/tmp" }, openrouterModel: "openrouter/minimax/minimax-m3" },
    deps,
  );
  assert.equal(calls.length, 1);
  assert.equal(
    calls[0].model,
    "minimax/minimax-m3",
    "expert dispatch must strip the openrouter/ prefix",
  );
});
