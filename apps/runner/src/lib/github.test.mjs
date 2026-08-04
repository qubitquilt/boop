import { test } from "node:test";
import assert from "node:assert/strict";

import {
  mintInstallationToken,
  postStatus,
  postReview,
  postInlineComments,
  cleanupPriorReview,
  makeOctokit,
  readWorkflowState,
  writeWorkflowState,
} from "./github.mjs";

const ctx = {
  prOwner: "qubitquilt",
  prRepo: "boop",
  prNumber: "42",
  prHeadSha: "0123456789abcdef0123456789abcdef01234567",
  prBaseRef: "main",
  previousHeadSha: null,
  statusCommentId: 100,
  reactionCommentId: 200,
  reviewNumber: 1,
  botLogin: "booppr[bot]",
};

// --- helpers ------------------------------------------------------------

function makeFakeOctokit(handlers = {}) {
  const calls = [];
  const rest = {
    issues: {
      getComment: handlers.getComment || (async () => ({ data: { body: "" } })),
      updateComment: handlers.updateComment || (async () => ({ data: {} })),
      createComment: handlers.createComment || (async () => ({ data: {} })),
    },
    pulls: {
      createReviewComment: handlers.createReviewComment || (async () => ({ data: {} })),
    },
  };
  return {
    rest,
    calls,
    issues: rest.issues,
    pulls: rest.pulls,
  };
}

function recordingLogger() {
  const log = [];
  return {
    log: (stage, msg, extra) => log.push({ level: "INFO", stage, msg, ...extra }),
    errlog: (stage, msg, extra) => log.push({ level: "ERROR", stage, msg, ...extra }),
    out: log,
  };
}

// --- makeOctokit -------------------------------------------------------

test("makeOctokit wraps the auth in an Octokit-shaped object", () => {
  const FakeOctokit = function (init) { this.init = init; };
  const oct = makeOctokit("tok", { OctokitCtor: FakeOctokit });
  assert.deepEqual(oct.init, { auth: "tok" });
});

// --- mintInstallationToken ---------------------------------------------

test("mintInstallationToken POSTs to /access_tokens and returns the token", async () => {
  const calls = [];
  const fetchImpl = async (url, opts) => {
    calls.push({ url, opts });
    return { ok: true, json: async () => ({ token: "ghs_inst_123" }) };
  };
  const deps = {
    jwt: { sign: () => "fake-jwt" },
    fetchImpl,
    log: () => {},
  };
  const tok = await mintInstallationToken("12345", "priv-key", "67890", deps);
  assert.equal(tok, "ghs_inst_123");
  assert.equal(calls.length, 1);
  assert.match(calls[0].url, /\/app\/installations\/67890\/access_tokens/);
  assert.equal(calls[0].opts.method, "POST");
  assert.equal(calls[0].opts.headers.Authorization, "Bearer fake-jwt");
});

test("mintInstallationToken throws on HTTP error", async () => {
  const fetchImpl = async () => ({ ok: false, status: 401, text: async () => "unauthorized" });
  const deps = {
    jwt: { sign: () => "fake-jwt" },
    fetchImpl,
    log: () => {},
  };
  await assert.rejects(
    () => mintInstallationToken("1", "k", "2", deps),
    /mint installation token: 401/,
  );
});

test("mintInstallationToken signs an RS256 JWT with the app id", async () => {
  let signedPayload;
  const deps = {
    jwt: { sign: (payload, key, opts) => {
      signedPayload = { payload, key, opts };
      return "fake-jwt";
    }},
    fetchImpl: async () => ({ ok: true, json: async () => ({ token: "x" }) }),
    log: () => {},
  };
  await mintInstallationToken("12345", "priv-key", "67890", deps);
  assert.equal(signedPayload.key, "priv-key");
  assert.equal(signedPayload.opts.algorithm, "RS256");
  assert.equal(signedPayload.payload.iss, "12345");
  // exp is 600s ahead of "now", and iat is 30s behind "now", so
  // exp - iat == 630.
  assert.equal(signedPayload.payload.exp - signedPayload.payload.iat, 630);
  assert.ok(signedPayload.payload.iat <= Math.floor(Date.now() / 1000));
});

// --- postStatus --------------------------------------------------------

test("postStatus skips when octokit or statusCommentId is missing", async () => {
  const log = recordingLogger();
  await postStatus("auth", undefined, { ...ctx, statusCommentId: null }, {
    octokit: makeFakeOctokit(),
    ...log,
  });
  // Second call: octokit is null.
  await postStatus("auth", undefined, ctx, { octokit: null, ...log });
  // The skip line should appear in log. The msg is the discriminator
  // here (the stage field is overlaid by extra in the logger helper).
  assert.ok(
    log.out.some((l) => /skip/.test(l.msg)),
    "expected a 'skip' log entry when octokit or statusCommentId missing",
  );
});

test("postStatus appends to existing timeline", async () => {
  const updates = [];
  const octokit = makeFakeOctokit({
    getComment: async () => ({
      data: { body: "header\n<!-- boop-timeline -->\n- 🥎 fetched" },
    }),
    updateComment: async (args) => {
      updates.push(args);
      return { data: {} };
    },
  });
  const log = recordingLogger();
  await postStatus("review", undefined, ctx, { octokit, ...log });
  assert.equal(updates.length, 1);
  assert.ok(updates[0].body.includes("<!-- boop-timeline -->"));
  assert.ok(updates[0].body.endsWith("- 👃 sniffing"));
});

test("postStatus prepends a fresh header + separator when no timeline yet", async () => {
  let captured;
  const octokit = makeFakeOctokit({
    getComment: async () => ({ data: { body: "header only, no timeline" } }),
    updateComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postStatus("auth", undefined, ctx, { octokit, log: () => {}, errlog: () => {} });
  assert.ok(captured.body.startsWith("🤝 **Paw-shaken in** — authenticated with GitHub at"));
  assert.ok(captured.body.includes("<!-- boop-timeline -->"));
});

test("postStatus renders the <details> block when detail is provided", async () => {
  let captured;
  const octokit = makeFakeOctokit({
    getComment: async () => ({ data: { body: "h\n<!-- boop-timeline -->\n- old" } }),
    updateComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postStatus("failed", "something went wrong", ctx, {
    octokit, log: () => {}, errlog: () => {},
  });
  assert.match(captured.body, /<details>/);
  assert.match(captured.body, /something went wrong/);
});

test("postStatus trims older entries when body exceeds 60KB", async () => {
  let captured;
  const longEntry = "x".repeat(200);
  const longBody = "h\n<!-- boop-timeline -->\n" + longEntry.repeat(500);
  const octokit = makeFakeOctokit({
    getComment: async () => ({ data: { body: longBody } }),
    updateComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postStatus("done", undefined, ctx, { octokit, log: () => {}, errlog: () => {} });
  assert.ok(captured.body.includes("earlier entries trimmed"));
  assert.ok(captured.body.length <= 60000);
});

test("postStatus swallows errors (best-effort)", async () => {
  const log = recordingLogger();
  const octokit = makeFakeOctokit({
    getComment: async () => { throw new Error("boom"); },
  });
  await postStatus("auth", undefined, ctx, { octokit, ...log });
  assert.ok(log.out.some((l) => l.level === "ERROR" && /comment update failed/.test(l.msg)));
});

// --- postReview --------------------------------------------------------

test("postReview posts summary with head-SHA marker", async () => {
  let captured;
  const octokit = makeFakeOctokit({
    createComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postReview(octokit, "## TL;DR\nLooks good.", 1, "high", ctx);
  assert.equal(captured.owner, "qubitquilt");
  assert.equal(captured.repo, "boop");
  assert.equal(captured.issue_number, 42);
  assert.match(captured.body, /## 🐾 Boop's review/);
  assert.match(captured.body, /✅ \*\*Confidence: high\*\*/);
  assert.match(captured.body, /<!-- boop-head-sha: 0123456789abcdef0123456789abcdef01234567 -->/);
});

test("postReview uses re-review header when reviewNumber > 1", async () => {
  let captured;
  const octokit = makeFakeOctokit({
    createComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postReview(octokit, "body", 3, "low", ctx);
  assert.match(captured.body, /## 🐾 Boop's re-review #3/);
  assert.match(captured.body, /🚨/);
  assert.match(captured.body, /review #3/);
});

test("postReview trims body to 65KB with truncation marker", async () => {
  let captured;
  const octokit = makeFakeOctokit({
    createComment: async (args) => { captured = args; return { data: {} }; },
  });
  const huge = "x".repeat(70000);
  await postReview(octokit, huge, 1, "medium", ctx);
  assert.ok(captured.body.length <= 65500);
  assert.match(captured.body, /\u2026\(truncated\)/);
});

test("postReview head marker contract (matches receiver regex)", () => {
  const sha = "87bcc09abcdef0123456789abcdef0123456789";
  const marker = `<!-- boop-head-sha: ${sha} -->`;
  assert.match(marker, /^<!--\s*boop-head-sha:\s*[0-9a-f]{7,40}\s*-->$/);
  assert.equal(marker.replace(/<!--\s*boop-head-sha:\s*([0-9a-f]{7,40})\s*-->/, "$1"), sha);
});

// --- postInlineComments ------------------------------------------------

test("postInlineComments posts each comment in parallel", async () => {
  const order = [];
  let inFlight = 0;
  let maxInFlight = 0;
  const octokit = makeFakeOctokit({
    createReviewComment: async (args) => {
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      await new Promise((r) => setTimeout(r, 5));
      inFlight--;
      order.push(args.path);
      return { data: {} };
    },
  });
  const log = recordingLogger();
  await postInlineComments(
    octokit,
    [
      { path: "a.ts", line: 1, body: "x" },
      { path: "b.ts", line: 2, body: "y" },
      { path: "c.ts", line: 3, body: "z" },
    ],
    ctx,
    log,
  );
  assert.deepEqual(order.sort(), ["a.ts", "b.ts", "c.ts"]);
  assert.ok(maxInFlight >= 2, `expected concurrent invocations, got max=${maxInFlight}`);
  assert.ok(log.out.some((l) => /posted 3\/3 inline comments/.test(l.msg)));
});

test("postInlineComments keeps going on individual failures", async () => {
  const octokit = makeFakeOctokit({
    createReviewComment: async (args) => {
      if (args.path === "b.ts") throw new Error("rate-limited");
      return { data: {} };
    },
  });
  const log = recordingLogger();
  await postInlineComments(
    octokit,
    [
      { path: "a.ts", line: 1, body: "x" },
      { path: "b.ts", line: 2, body: "y" },
      { path: "c.ts", line: 3, body: "z" },
    ],
    ctx,
    log,
  );
  assert.ok(log.out.some((l) => /posted 2\/3/.test(l.msg)));
  assert.ok(log.out.some((l) => l.level === "ERROR" && /failed to post inline comment/.test(l.msg)));
});

test("postInlineComments does nothing on empty list", async () => {
  const octokit = makeFakeOctokit();
  const log = recordingLogger();
  await postInlineComments(octokit, [], ctx, log);
  assert.equal(log.out.filter((l) => /inline/.test(l.msg)).length, 0);
});

// --- cleanupPriorReview -----------------------------------------------

function makeScriptedFetch(scripts) {
  const calls = [];
  const fetchImpl = async (url, opts) => {
    calls.push({ url, opts });
    if (scripts.length === 0) {
      return { ok: true, json: async () => ({}), text: async () => "" };
    }
    const next = scripts.shift();
    if (typeof next === "function") return next(url, opts);
    return next;
  };
  fetchImpl.calls = calls;
  return fetchImpl;
}

test("cleanupPriorReview resolves outdated bot threads and minimizes prior comments", async () => {
  // ctx.statusCommentId is 100 — use different ids for the prior
  // comments so they're not filtered out as the current status.
  // Promise.all launches both paginators in parallel; the order of
  // fetches is:
  //   1. fetchAllReviewThreads page 1 (1 thread)
  //   2. fetchPriorBotIssueCommentIDs page 1 (3 items, breaks — <100)
  //   3. resolveReviewThread for T1
  //   4. minimizeComment for N1
  //   5. minimizeComment for N2
  const fetchImpl = makeScriptedFetch([
    // 1. review threads page 1
    {
      ok: true,
      json: async () => ({
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: false, endCursor: null },
                nodes: [
                  { id: "T1", isResolved: false, isOutdated: true, comments: { nodes: [{ author: { login: "booppr[bot]" } }] } },
                  { id: "T2", isResolved: false, isOutdated: true, comments: { nodes: [{ author: { login: "alice" } }] } },
                  { id: "T3", isResolved: true, isOutdated: true, comments: { nodes: [{ author: { login: "booppr[bot]" } }] } },
                ],
              },
            },
          },
        },
      }),
    },
    // 2. issue comments page 1 (3 items, breaks because < 100)
    {
      ok: true,
      json: async () => ([
        { id: 1, node_id: "N1", user: { login: "booppr[bot]" } },
        { id: 2, node_id: "N2", user: { login: "booppr[bot]" } },
        { id: 3, node_id: "N3", user: { login: "alice" } },
      ]),
    },
    // 3. resolveReviewThread for T1
    {
      ok: true,
      json: async () => ({
        data: { resolveReviewThread: { thread: { id: "T1", isResolved: true } } },
      }),
    },
    // 4. minimizeComment for N1
    {
      ok: true,
      json: async () => ({
        data: { minimizeComment: { minimizedComment: { isMinimized: true } } },
      }),
    },
    // 5. minimizeComment for N2
    {
      ok: true,
      json: async () => ({
        data: { minimizeComment: { minimizedComment: { isMinimized: true } } },
      }),
    },
  ]);
  const log = recordingLogger();
  const result = await cleanupPriorReview("tok", ctx, { fetchImpl, ...log });
  assert.deepEqual(result, { resolved: 1, minimized: 2, errors: 0 });
});

test("cleanupPriorReview paginates review threads and issue comments", async () => {
  // Fetch order (observed empirically — `fetchAllReviewThreads`
  // wraps fetchImpl in an extra async hop via `graphql`, which
  // interleaves the second reviewThreads fetch AFTER the second
  // issueComments fetch):
  //   1. reviewThreads page 1 (1 thread, hasNextPage=true)
  //   2. issueComments page 1 (100 items, signals more pages)
  //   3. issueComments page 2 (empty, signals end)
  //   4. reviewThreads page 2 (1 thread, hasNextPage=false)
  //   5. resolveReviewThread for T1
  //   6. resolveReviewThread for T2
  //   7..106. minimizeComment x 100
  const fetchImpl = makeScriptedFetch([
    // 1. review threads page 1
    {
      ok: true,
      json: async () => ({
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: true, endCursor: "C1" },
                nodes: [
                  { id: "T1", isResolved: false, isOutdated: true, comments: { nodes: [{ author: { login: "booppr[bot]" } }] } },
                ],
              },
            },
          },
        },
      }),
    },
    // 2. issue comments page 1 — full page of 100
    {
      ok: true,
      json: async () => {
        const arr = Array.from({ length: 100 }, (_, i) => ({
          id: 1000 + i, node_id: `N${i}`, user: { login: "booppr[bot]" },
        }));
        return arr;
      },
    },
    // 3. issue comments page 2 — empty (signals end)
    { ok: true, json: async () => [] },
    // 4. review threads page 2
    {
      ok: true,
      json: async () => ({
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: false, endCursor: null },
                nodes: [
                  { id: "T2", isResolved: false, isOutdated: true, comments: { nodes: [{ author: { login: "booppr[bot]" } }] } },
                ],
              },
            },
          },
        },
      }),
    },
    // 5. resolveReviewThread for T1
    {
      ok: true,
      json: async () => ({
        data: { resolveReviewThread: { thread: { id: "T1", isResolved: true } } },
      }),
    },
    // 6. resolveReviewThread for T2
    {
      ok: true,
      json: async () => ({
        data: { resolveReviewThread: { thread: { id: "T2", isResolved: true } } },
      }),
    },
    // 7..106. minimizeComment x 100
    ...Array.from({ length: 100 }, () => ({
      ok: true,
      json: async () => ({
        data: { minimizeComment: { minimizedComment: { isMinimized: true } } },
      }),
    })),
  ]);
  const log = recordingLogger();
  const result = await cleanupPriorReview("tok", ctx, { fetchImpl, ...log });
  assert.equal(result.resolved, 2);
  assert.equal(result.minimized, 100);
  assert.equal(result.errors, 0);
});

test("cleanupPriorReview counts errors but keeps going", async () => {
  // Fetch order:
  //   1. reviewThreads page 1
  //   2. issueComments page 1 (1 item, breaks)
  //   3. resolveReviewThread for T1 — fails
  //   4. minimizeComment for N1 — succeeds
  const fetchImpl = makeScriptedFetch([
    {
      ok: true,
      json: async () => ({
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: false, endCursor: null },
                nodes: [
                  { id: "T1", isResolved: false, isOutdated: true, comments: { nodes: [{ author: { login: "booppr[bot]" } }] } },
                ],
              },
            },
          },
        },
      }),
    },
    {
      ok: true,
      json: async () => ([
        { id: 1, node_id: "N1", user: { login: "booppr[bot]" } },
      ]),
    },
    // resolveReviewThread fails
    {
      ok: false,
      status: 500,
      text: async () => "boom",
    },
    // minimizeComment succeeds
    {
      ok: true,
      json: async () => ({
        data: { minimizeComment: { minimizedComment: { isMinimized: true } } },
      }),
    },
  ]);
  const log = recordingLogger();
  const result = await cleanupPriorReview("tok", ctx, { fetchImpl, ...log });
  assert.equal(result.resolved, 0);
  assert.equal(result.minimized, 1);
  assert.equal(result.errors, 1);
  assert.ok(log.out.some((l) => l.level === "ERROR" && /resolve failed/.test(l.msg)));
});

test("cleanupPriorReview skips bot filter when ctx.botLogin is null", async () => {
  const noBotCtx = { ...ctx, botLogin: null };
  const fetchImpl = makeScriptedFetch([
    {
      ok: true,
      json: async () => ({
        data: {
          repository: {
            pullRequest: {
              reviewThreads: {
                pageInfo: { hasNextPage: false, endCursor: null },
                nodes: [
                  { id: "T1", isResolved: false, isOutdated: true, comments: { nodes: [{ author: { login: "booppr[bot]" } }] } },
                ],
              },
            },
          },
        },
      }),
    },
    { ok: true, json: async () => [] },
  ]);
  const log = recordingLogger();
  const result = await cleanupPriorReview("tok", noBotCtx, { fetchImpl, ...log });
  assert.equal(result.resolved, 0);
  assert.equal(result.minimized, 0);
});

// --- workflow state read / write (QUB-92) -----------------------------

test("readWorkflowState returns the passed list from the comment body", async () => {
  const octokit = makeFakeOctokit({
    getComment: async () => ({
      data: {
        body:
          "🐾 header\n\n<!-- boop-timeline -->\n- 👃 sniffing\n<!-- boop-state: {\"passed\":[\"handshake\",\"fetch\"],\"sub\":{\"sniff\":[\"classify\"]}} -->\n",
      },
    }),
  });
  const deps = { log: () => {}, errlog: () => {} };
  const result = await readWorkflowState(octokit, ctx, deps);
  assert.deepEqual(result.passed, ["handshake", "fetch"]);
  assert.deepEqual(result.sub, { sniff: ["classify"] });
});

test("readWorkflowState returns empty state when the marker is missing", async () => {
  const octokit = makeFakeOctokit({
    getComment: async () => ({
      data: { body: "🐾 header\n\n<!-- boop-timeline -->\n" },
    }),
  });
  const deps = { log: () => {}, errlog: () => {} };
  const result = await readWorkflowState(octokit, ctx, deps);
  assert.deepEqual(result, { passed: [], sub: {} });
});

test("readWorkflowState treats malformed JSON as a fresh run", async () => {
  const octokit = makeFakeOctokit({
    getComment: async () => ({
      data: { body: "<!-- boop-state: {not-json} -->" },
    }),
  });
  const deps = { log: () => {}, errlog: () => {} };
  const result = await readWorkflowState(octokit, ctx, deps);
  assert.deepEqual(result, { passed: [], sub: {} });
});

test("readWorkflowState returns empty state when octokit is missing", async () => {
  // A test that drives the runner without a real Octokit
  // gets an empty state — same as a fresh run.
  const deps = { log: () => {}, errlog: () => {} };
  const result = await readWorkflowState(null, { ...ctx, statusCommentId: 100 }, deps);
  assert.deepEqual(result, { passed: [], sub: {} });
});

test("writeWorkflowState PATCHes the comment with the state line", async () => {
  // A fresh body (no prior state) gets the state appended.
  let captured = null;
  const octokit = makeFakeOctokit({
    getComment: async () => ({ data: { body: "header\n" } }),
    updateComment: async (args) => {
      captured = args;
      return { data: { id: 1, body: args.body } };
    },
  });
  const log = recordingLogger();
  await writeWorkflowState(
    octokit,
    ctx,
    { log: log.log, errlog: log.errlog },
    { passed: ["handshake"], sub: {} },
  );
  assert.ok(captured, "updateComment should have been called");
  assert.match(captured.body, /boop-state:.*"passed":\["handshake"\]/);
});

test("writeWorkflowState replaces the prior state line on a re-write", async () => {
  // The runner writes the state many times during a single
  // run (once per macro stage). The marker is upserted in
  // place; the rest of the comment body is preserved.
  let captured = null;
  const octokit = makeFakeOctokit({
    getComment: async () => ({
      data: {
        body:
          "header\n<!-- boop-state: {\"passed\":[\"handshake\"],\"sub\":{}} -->\n",
      },
    }),
    updateComment: async (args) => {
      captured = args;
      return { data: { id: 1, body: args.body } };
    },
  });
  await writeWorkflowState(
    octokit,
    ctx,
    { log: () => {}, errlog: () => {} },
    { passed: ["handshake", "fetch"], sub: {} },
  );
  assert.ok(captured, "updateComment should have been called");
  assert.match(captured.body, /boop-state:.*"passed":\["handshake","fetch"\]/);
  // The header is preserved.
  assert.match(captured.body, /^header\n/);
  // Only one state line (the upsert replaced the old one).
  const matches = captured.body.match(/<!-- boop-state:/g) || [];
  assert.equal(matches.length, 1);
});

test("writeWorkflowState swallows errors (best-effort)", async () => {
  // A failed PATCH does not throw; the run continues. The
  // state is best-effort — a re-trigger's state-read will see
  // the last successful write.
  const octokit = makeFakeOctokit({
    getComment: async () => { throw new Error("get failed"); },
  });
  const log = recordingLogger();
  // No throw:
  await writeWorkflowState(
    octokit,
    ctx,
    { log: log.log, errlog: log.errlog },
    { passed: ["handshake"], sub: {} },
  );
  assert.ok(
    log.out.some(
      (e) => e.level === "ERROR" && /get failed/.test(e.err || ""),
    ),
  );
});
