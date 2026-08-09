import { test } from "node:test";
import assert from "node:assert/strict";

import {
  pickExperts,
  EXPERT_POOL,
  runExperts,
  gather,
  defaultNarrate,
  placeholderNarrate,
  _PLACEHOLDER_NARRATE_SUMMARY,
} from "./experts.mjs";
import { parseReviewOutput } from "./openrouter.mjs";

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

// QUB-120: the multi-expert dispatch must forward
// OPENROUTER_API_KEY to callOpenRouter. The runner loads the
// key from a mounted Secret file into state.openrouterApiKey
// (workflow.mjs:590) and threads it through deps.env
// (workflow.mjs:591-600). The expert dispatch spreads ...deps
// into callOpenRouter (experts.mjs:233), so deps.env lands in
// the SDK call. Without this, callOpenRouter defaults to
// process.env (openrouter.mjs:172) and the key is unset — the
// runner never exports the file-loaded secret into the
// process environment — so the guard at openrouter.mjs:188-191
// fires on every expert dispatch. This test pins the contract:
// whatever the runner puts on deps.env.OPENROUTER_API_KEY
// reaches callOpenRouter verbatim.
test("defaultExpert forwards deps.env.OPENROUTER_API_KEY to callOpenRouter (QUB-120)", async () => {
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
    // Simulate what the handshake stage does: thread the
    // file-loaded key through deps.env so the multi-expert
    // spread picks it up.
    env: { OPENROUTER_API_KEY: "test-key-from-secret-file" },
    log: () => {},
    errlog: () => {},
  };
  await runExperts(
    ["regression-hunter"],
    { paths: { configSrc: "/tmp" }, openrouterModel: "minimax/minimax-m3" },
    deps,
  );
  assert.equal(calls.length, 1, "expert dispatch must invoke callOpenRouter exactly once");
  assert.ok(calls[0], "expert dispatch must capture opts");
  assert.ok(
    calls[0].env && typeof calls[0].env === "object",
    "expert dispatch must forward an env object to callOpenRouter",
  );
  assert.equal(
    calls[0].env.OPENROUTER_API_KEY,
    "test-key-from-secret-file",
    "expert dispatch must forward deps.env.OPENROUTER_API_KEY verbatim",
  );
  assert.ok(
    typeof calls[0].env.OPENROUTER_API_KEY === "string" &&
      calls[0].env.OPENROUTER_API_KEY.length > 0,
    "OPENROUTER_API_KEY must be a non-empty string",
  );
});

// QUB-<next> local-run regression: the lens file path must be
// resolved from deps.paths.configSrc, NOT ctx.paths.configSrc
// (ctx never carries `paths` — only deps does). The pre-fix code
// read ctx.paths.configSrc, which is always undefined, so the
// hard-coded "/home/opencode/.config/opencode" fallback always
// fired. The K8s production mount happens to match that path,
// so the bug stayed invisible until BOOP_CONFIG_SRC env-override
// local runs redirected the mount elsewhere — every expert then
// failed with ENOENT against /home/opencode.
//
// The test pins that a custom deps.paths.configSrc is honored
// (so the local run reads /tmp/boop-runner/skills/boop/...),
// and that the fallback fires when deps.paths.configSrc is absent.
test("defaultExpert reads lens path from deps.paths.configSrc (QUB-<next>)", async () => {
  // Capture the path the expert tried to read. fs.readFile is the
  // ground-truth source the expert uses (the rtk adapter falls
  // back to fs when its CLI call fails).
  const reads = [];
  const baseDeps = (configSrc) => ({
    callOpenRouter: async () => ({
      text: '{"findings": []}',
      model: "test-model",
      usage: { prompt_tokens: 0, completion_tokens: 0, cost: 0 },
    }),
    fs: {
      readFile: async (p) => {
        reads.push(p);
        return "# test lens\nReturn {findings:[]}.";
      },
    },
    paths: { configSrc },
    postStatus: async () => {},
    env: { OPENROUTER_API_KEY: "test-key" },
    log: () => {},
    errlog: () => {},
  });

  // Case 1: a custom configSrc (the BOOP_CONFIG_SRC override) must
  // be honored. The expert reads from /tmp/boop-runner/skills,
  // not from the production mount.
  reads.length = 0;
  await runExperts(
    ["regression-hunter"],
    { openrouterModel: "minimax/minimax-m3" },
    baseDeps("/tmp/boop-runner"),
  );
  assert.ok(
    reads.some((p) => p.includes("/tmp/boop-runner/skills/boop/")),
    `expert must read from custom configSrc; got reads: ${JSON.stringify(reads)}`,
  );
  assert.ok(
    !reads.some((p) => p.startsWith("/home/opencode")),
    `expert must NOT fall back to production mount; got reads: ${JSON.stringify(reads)}`,
  );

  // Case 2: when deps.paths.configSrc is absent, the expert
  // falls back to the production mount (preserves the pre-fix
  // behavior for any caller that doesn't thread paths through).
  reads.length = 0;
  const depsNoPaths = baseDeps("/some/other/path");
  delete depsNoPaths.paths;
  await runExperts(
    ["regression-hunter"],
    { openrouterModel: "minimax/minimax-m3" },
    depsNoPaths,
  );
  assert.ok(
    reads.some((p) => p.startsWith("/home/opencode/.config/opencode/")),
    `expert must fall back to /home/opencode when deps.paths.configSrc is absent; got: ${JSON.stringify(reads)}`,
  );
});

// --- QUB-130: placeholderNarrate ---------------------------------------
//
// When the multi-expert dispatch returns 0 findings, the
// narrator's LLM can refuse to produce a review (the model
// emits a refusal under 200 bytes that the parser rejects as
// "summary empty"). The placeholder bypasses the LLM and
// produces a clean, deterministic "no issues found" review
// that passes the parser's shape check.

test("placeholderNarrate returns the same summary regardless of walkthrough (QUB-130)", () => {
  // The placeholder body is intentionally generic. A
  // walkthrough-shaped failure must not leak into the
  // review. The function accepts walkthrough /
  // walkthroughIsPlaceholder for diagnostic logging only.
  const a = placeholderNarrate([], "any walkthrough", true, {}, { log: () => {} });
  const b = placeholderNarrate(
    [],
    "another walkthrough that is completely different",
    false,
    {},
    { log: () => {} },
  );
  assert.equal(a.summary, b.summary, "placeholder body is identical for any walkthrough");
});

test("placeholderNarrate returns a summary that passes parseReviewOutput (QUB-130)", () => {
  // The placeholder summary must pass the parser's
  // looksLikeReviewShape gate (≥ 200 bytes, has a heading
  // or finding table, no refusal patterns). The runner
  // posts the summary to the PR via the structured block;
  // a failure here would mean the placeholder is rejected
  // by the same gate that rejected the LLM's refusal.
  const placeholder = placeholderNarrate([], "", true, {}, { log: () => {} });
  // Round-trip the summary through the full structured
  // block to exercise parseReviewOutput's shape check.
  const wrapped = [
    "=== SUMMARY ===",
    placeholder.summary,
    "=== INLINE COMMENTS ===",
    "=== CONFIDENCE ===",
    placeholder.confidence,
    "=== END ===",
  ].join("\n");
  const parsed = parseReviewOutput(wrapped);
  assert.equal(parsed.parseError, null, `placeholder failed parse: ${parsed.parseError}`);
  assert.equal(parsed.summary, placeholder.summary);
  assert.equal(parsed.confidence, "high");
  assert.equal(parsed.inlineComments.length, 0);
});

test("placeholderNarrate stamps stepCount: 0 on telemetry (QUB-130)", () => {
  // The dashboard distinguishes a placeholder review from
  // a successful LLM review via stepCount (0 vs 1). The
  // telemetry shape matches emptyTelemetry so the row
  // looks like a zero-cost call.
  const placeholder = placeholderNarrate([], "", false, {}, { log: () => {} });
  assert.equal(placeholder.telemetry.stepCount, 0);
  assert.equal(placeholder.telemetry.provider, "openrouter");
  assert.equal(placeholder.telemetry.inputTokens, 0);
  assert.equal(placeholder.telemetry.outputTokens, 0);
  assert.equal(placeholder.telemetry.costUsd, 0);
});

test("placeholderNarrate returns high confidence and empty inline comments (QUB-130)", () => {
  const placeholder = placeholderNarrate([], "", true, {}, { log: () => {} });
  assert.equal(placeholder.confidence, "high");
  assert.deepEqual(placeholder.inlineComments, []);
});

test("placeholderNarrate summary is locked to the documented body (QUB-130)", () => {
  // The summary string is consumed by tests + the runner.
  // A future refactor that changes the wording is a user-
  // visible change and needs an explicit decision. Pin the
  // exported constant so the assertion covers the literal
  // string the placeholder posts.
  const placeholder = placeholderNarrate([], "", true, {}, { log: () => {} });
  assert.equal(placeholder.summary, _PLACEHOLDER_NARRATE_SUMMARY);
});
