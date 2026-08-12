import { test } from "node:test";
import assert from "node:assert/strict";

import {
  mintInstallationToken,
  postStatus,
  postFinalReaction,
  postReview,
  postInlineComments,
  cleanupPriorReview,
  makeOctokit,
  ensureStatusComment,
  renderInitialStatusBody,
  readWorkflowState,
  writeWorkflowState,
  inlineKeyForComment,
  appendInlineKeyMarker,
  parseInlineKey,
  listExistingInlineKeys,
} from "./github.ts";

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
  reviewId: "test-review-uuid",
};

// --- helpers ------------------------------------------------------------

function makeFakeOctokit(handlers = {}) {
  const calls = [];
  const rest = {
    issues: {
      getComment: async (args) => {
        calls.push({ getComment: args });
        return (handlers.getComment || (async () => ({ data: { body: "" } })))(args);
      },
      updateComment: async (args) => {
        calls.push({ updateComment: args });
        return (handlers.updateComment || (async () => ({ data: {} })))(args);
      },
      createComment: async (args) => {
        calls.push({ createComment: args });
        return (handlers.createComment || (async () => ({ data: { id: 555 } })))(args);
      },
      listComments: async (args) => {
        calls.push({ listComments: args });
        return (handlers.listComments || (async () => ({ data: [] })))(args);
      },
    },
    pulls: {
      createReviewComment: async (args) => {
        calls.push({ createReviewComment: args });
        return (handlers.createReviewComment || (async () => ({ data: {} })))(args);
      },
      listReviewComments: async (args) => {
        calls.push({ listReviewComments: args });
        return (handlers.listReviewComments || (async () => ({ data: [] })))(args);
      },
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

// QUB-114: in reaction mode (issue_comment trigger), postStatus
// is a no-op regardless of the other inputs. The runner adds a
// single terminal reaction at the end of the run instead.
test("postStatus skips when noStatusComment is true (QUB-114 reaction mode)", async () => {
  const log = recordingLogger();
  const updateCalls = [];
  const octokit = {
    rest: {
      issues: {
        getComment: async () => ({ data: { body: "current body" } }),
        updateComment: async (args) => {
          updateCalls.push(args);
          return { data: { id: 1 } };
        },
        createComment: async () => ({ data: { id: 1 } }),
      },
    },
  };
  await postStatus(
    "auth",
    undefined,
    { ...ctx, noStatusComment: true, statusCommentId: 111 },
    { octokit, ...log },
  );
  assert.equal(updateCalls.length, 0, "postStatus must not PATCH the status comment in reaction mode");
  assert.ok(
    log.out.some((l) => /reaction mode/.test(l.msg)),
    "expected a 'reaction mode' skip log entry",
  );
});

// QUB-114: postFinalReaction adds the terminal reaction to
// the trigger comment on done (hooray) or failed (-1). The
// function resolves the octokit via deps.getOctokit?.() so
// the live runner's slot-only deps shape works (the prior
// version destructured deps.octokit and silently no-op'd).

function reactionFakeOctokit(contentToCapture) {
  const calls = [];
  return {
    calls,
    rest: {
      reactions: {
        createForIssueComment: async (args) => {
          calls.push(args);
          contentToCapture.value = args.content;
          return { data: { id: 1 } };
        },
      },
    },
  };
}

test("postFinalReaction adds hooray reaction on done (deps.getOctokit slot)", async () => {
  // This is the bug the reviewer caught: deps in the live
  // runner exposes getOctokit (a function), not octokit (the
  // instance). The function must resolve via the slot.
  const log = recordingLogger();
  const octokit = reactionFakeOctokit({ value: null });
  const deps = { log: log.log, errlog: log.errlog, getOctokit: () => octokit };
  await postFinalReaction("done", { ...ctx, reactionCommentId: 200 }, deps);
  assert.equal(octokit.calls.length, 1);
  assert.equal(octokit.calls[0].content, "hooray");
  assert.equal(octokit.calls[0].comment_id, 200);
  assert.equal(octokit.calls[0].owner, ctx.prOwner);
  assert.equal(octokit.calls[0].repo, ctx.prRepo);
  assert.ok(
    log.out.some((l) => /final reaction added/.test(l.msg)),
    "expected a 'final reaction added' log entry",
  );
});

test("postFinalReaction adds -1 reaction on failed", async () => {
  const log = recordingLogger();
  const octokit = reactionFakeOctokit({ value: null });
  const deps = { log: log.log, errlog: log.errlog, getOctokit: () => octokit };
  await postFinalReaction("failed", { ...ctx, reactionCommentId: 201 }, deps);
  assert.equal(octokit.calls.length, 1);
  assert.equal(octokit.calls[0].content, "-1");
  assert.equal(octokit.calls[0].comment_id, 201);
});

test("postFinalReaction falls back to deps.octokit when getOctokit is missing", async () => {
  // The function must work with both shapes: the live
  // runner passes a slot (deps.getOctokit), test fixtures
  // sometimes pass the octokit directly (deps.octokit).
  const log = recordingLogger();
  const octokit = reactionFakeOctokit({ value: null });
  const deps = { log: log.log, errlog: log.errlog, octokit };
  await postFinalReaction("done", { ...ctx, reactionCommentId: 202 }, deps);
  assert.equal(octokit.calls.length, 1);
  assert.equal(octokit.calls[0].content, "hooray");
  assert.equal(octokit.calls[0].comment_id, 202);
});

test("postFinalReaction skips when no octokit is available", async () => {
  // Reaction mode is the only path that calls this; the
  // runner should still not throw if the handshake never
  // ran (octokit is null). Best-effort: log + skip.
  const log = recordingLogger();
  const deps = { log: log.log, errlog: log.errlog, getOctokit: () => null };
  await postFinalReaction(
    "done",
    { ...ctx, reactionCommentId: 203 },
    deps,
  );
  assert.ok(
    log.out.some((l) => /skip final reaction/.test(l.msg)),
    "expected a 'skip' log entry when octokit is null",
  );
});

test("postFinalReaction skips when reactionCommentId is missing (pull_request path)", async () => {
  // pull_request-driven runs do not have a trigger
  // comment; reactionCommentId is empty. The function is a
  // no-op there.
  const log = recordingLogger();
  const octokit = reactionFakeOctokit({ value: null });
  const deps = { log: log.log, errlog: log.errlog, getOctokit: () => octokit };
  await postFinalReaction(
    "done",
    { ...ctx, reactionCommentId: null },
    deps,
  );
  assert.equal(octokit.calls.length, 0, "no reaction when no trigger comment");
  assert.ok(
    log.out.some((l) => /skip final reaction/.test(l.msg)),
    "expected a 'skip' log entry when reactionCommentId is empty",
  );
});

test("postFinalReaction swallows API errors (best-effort)", async () => {
  // The function must not fail the run on a reaction API
  // error. Errors are logged.
  const log = recordingLogger();
  const calls = [];
  const octokit = {
    rest: {
      reactions: {
        createForIssueComment: async () => {
          calls.push("called");
          throw new Error("rate-limited");
        },
      },
    },
  };
  const deps = { log: log.log, errlog: log.errlog, getOctokit: () => octokit };
  await postFinalReaction(
    "done",
    { ...ctx, reactionCommentId: 204 },
    deps,
  );
  assert.equal(calls.length, 1);
  assert.ok(
    log.out.some(
      (l) => l.level === "ERROR" && /final reaction failed/.test(l.msg),
    ),
    "expected a 'final reaction failed' error log entry",
  );
});

test("postFinalReaction skips on unknown stage", async () => {
  const log = recordingLogger();
  const octokit = reactionFakeOctokit({ value: null });
  const deps = { log: log.log, errlog: log.errlog, getOctokit: () => octokit };
  await postFinalReaction(
    "review", // not in the {done, failed} set
    { ...ctx, reactionCommentId: 205 },
    deps,
  );
  assert.equal(octokit.calls.length, 0);
  assert.ok(
    log.out.some((l) => /unknown stage/.test(l.msg)),
    "expected an 'unknown stage' log entry",
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

// --- postReview QUB-103 idempotency ------------------------------------

test("postReview carries the per-run review-id marker (QUB-103)", async () => {
  // QUB-103: every summary comment embeds a per-run UUID the
  // runner generated at run() start. The marker lets postReview
  // find the existing summary on retry and PATCH it instead of
  // double-posting. The marker is hidden HTML (renders as nothing
  // in the markdown view) and survives the GitHub API verbatim.
  let captured;
  const octokit = makeFakeOctokit({
    createComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postReview(octokit, "body", 1, "high", ctx);
  assert.match(
    captured.body,
    /<!-- boop-review-id: test-review-uuid -->/,
    "summary body must carry the per-run review-id marker",
  );
});

test("postReview PATCHes existing summary when matching review-id is found (QUB-103)", async () => {
  // Headline QUB-103 dedup path: a prior run (or a sibling pod)
  // posted a summary with this run's review-id marker. The next
  // call must PATCH it in place, not create a second comment.
  // Without this, a pod kill mid-run would produce duplicate
  // summary comments on the PR.
  let updatedCommentId;
  let createCalls = 0;
  const octokit = makeFakeOctokit({
    listComments: async () => ({
      data: [
        // Older, unrelated summary (different review id) — not a
        // match.
        { id: 900, body: "old summary\n<!-- boop-review-id: other-run -->\n<!-- boop-head-sha: aaaa -->\n" },
        // The current run's prior summary — match by review-id.
        { id: 123, body: "prior summary\n<!-- boop-review-id: test-review-uuid -->\n<!-- boop-head-sha: 0123456789abcdef0123456789abcdef01234567 -->\n" },
      ],
    }),
    updateComment: async (args) => {
      updatedCommentId = args.comment_id;
      return { data: {} };
    },
    createComment: async () => { createCalls++; return { data: { id: 999 } }; },
  });
  const log = recordingLogger();
  await postReview(octokit, "new summary body", 1, "high", ctx, log);
  assert.equal(updatedCommentId, 123, "must PATCH the matching comment, not create a new one");
  assert.equal(createCalls, 0, "no createComment call when a match exists");
  assert.ok(
    log.out.some((l) => /patched existing summary comment/.test(l.msg)),
    "expected a 'patched' log entry",
  );
});

test("postReview falls back to head-SHA match when no review-id match (QUB-103 / pre-QUB-103 summaries)", async () => {
  // Pre-QUB-103 summaries don't carry the review-id marker. The
  // dedup must still work via the head-SHA marker so an upgrade
  // from an older runner doesn't suddenly start posting
  // duplicates. The match is the most recent (last in the
  // chronological list) comment whose body contains the head
  // SHA — not just any matching comment.
  let updatedCommentId;
  const octokit = makeFakeOctokit({
    listComments: async () => ({
      data: [
        { id: 800, body: "old\n<!-- boop-head-sha: aaaa1111 -->\n" },
        // Match by head-SHA only (older form, no review-id).
        { id: 456, body: "older summary\n<!-- boop-head-sha: 0123456789abcdef0123456789abcdef01234567 -->\n" },
      ],
    }),
    updateComment: async (args) => {
      updatedCommentId = args.comment_id;
      return { data: {} };
    },
    createComment: async () => { throw new Error("must not create when head-SHA match exists"); },
  });
  await postReview(octokit, "body", 1, "high", ctx);
  assert.equal(updatedCommentId, 456, "must PATCH the head-SHA-matched comment");
});

test("postReview picks the most recent match across pagination (QUB-103)", async () => {
  // GitHub returns comments oldest-first. A duplicated
  // (head-SHA-matching) summary on an earlier page should NOT
  // win over a newer one on a later page. The dedup walks the
  // whole pagination and returns the highest index. Without
  // this, a partially-completed retry could PATCH the older
  // duplicate and leave the newer one stale.
  let updatedCommentId;
  const octokit = makeFakeOctokit({
    listComments: async (args) => {
      if (args.page === 1) {
        // First page is full (100 items, signals more pages).
        // The match on this page is older (id 111).
        const filler = Array.from({ length: 99 }, (_, i) => ({
          id: 1000 + i,
          body: "filler\n",
        }));
        return {
          data: [
            { id: 111, body: "older\n<!-- boop-head-sha: 0123456789abcdef0123456789abcdef01234567 -->\n" },
            ...filler,
          ],
        };
      }
      // Page 2 — fewer items (last page). The match here is
      // newer (id 222); the dedup must pick it.
      return {
        data: [
          { id: 222, body: "newer\n<!-- boop-head-sha: 0123456789abcdef0123456789abcdef01234567 -->\n" },
        ],
      };
    },
    updateComment: async (args) => {
      updatedCommentId = args.comment_id;
      return { data: {} };
    },
    createComment: async () => { throw new Error("must not create when match exists"); },
  });
  await postReview(octokit, "body", 1, "high", ctx);
  assert.equal(updatedCommentId, 222, "must PATCH the most recent match");
});

test("postReview creates a fresh summary when no existing marker matches (QUB-103)", async () => {
  // No prior comment carries the review-id or head-SHA marker.
  // The runner posts a fresh comment. This is the first-run
  // happy path; the test pins that the dedup machinery doesn't
  // accidentally skip the create.
  let createCalls = 0;
  let updateCalls = 0;
  const octokit = makeFakeOctokit({
    listComments: async () => ({
      data: [
        // Different head SHA — not a match.
        { id: 999, body: "prior run on a different commit\n<!-- boop-head-sha: deadbeef00000000 -->\n" },
      ],
    }),
    createComment: async () => { createCalls++; return { data: { id: 555 } }; },
    updateComment: async () => { updateCalls++; return { data: {} }; },
  });
  const log = recordingLogger();
  await postReview(octokit, "body", 1, "high", ctx, log);
  assert.equal(createCalls, 1, "must createComment when no match exists");
  assert.equal(updateCalls, 0, "must not updateComment when no match exists");
  assert.ok(log.out.some((l) => /created summary comment/.test(l.msg)));
});

test("postReview falls back to a fresh POST when the listing throws 502 (QUB-103)", async () => {
  // The dedup is best-effort. A 502 on listComments cannot cost
  // the run — postReview swallows the listing error and posts
  // fresh. Worst case is a duplicate summary (caught by the
  // workflow-state abort path on the next pod).
  let createCalls = 0;
  const octokit = makeFakeOctokit({
    listComments: async () => { throw new Error("502 Bad Gateway"); },
    createComment: async () => { createCalls++; return { data: { id: 555 } }; },
  });
  const log = recordingLogger();
  await postReview(octokit, "body", 1, "high", ctx, log);
  assert.equal(createCalls, 1, "must fall back to createComment when list fails");
  assert.ok(
    log.out.some((l) => /list existing summary comments failed/.test(l.msg)),
    "expected a 'list failed' log entry",
  );
});

test("postReview head marker is preserved alongside the review-id marker (QUB-103)", async () => {
  // Both markers must land in the comment body. The receiver's
  // priorReviewHeadSHARegex still parses the head SHA; the
  // review-id marker is additive, not a replacement. Removing
  // the head-SHA marker would silently break the receiver's
  // re-review diffing (priorReviewHeadSHARegex depends on it).
  let captured;
  const octokit = makeFakeOctokit({
    createComment: async (args) => { captured = args; return { data: {} }; },
  });
  await postReview(octokit, "body", 1, "high", ctx);
  assert.match(captured.body, /<!-- boop-head-sha: 0123456789abcdef0123456789abcdef01234567 -->/);
  assert.match(captured.body, /<!-- boop-review-id: test-review-uuid -->/);
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

// --- postInlineComments QUB-103 idempotency -----------------------------

test("postInlineComments appends the inline-key marker to every body (QUB-103)", async () => {
  // Each posted body must carry the deterministic
  // `<!-- boop-inline: <path>:<line>:<hash> -->` marker. The
  // marker survives the GitHub API verbatim; on a retry the
  // runner reads the existing review comments' markers to
  // detect duplicates. Without the marker on every body, a
  // re-run cannot tell a posted comment from a candidate.
  const postedBodies = [];
  const octokit = makeFakeOctokit({
    createReviewComment: async (args) => {
      postedBodies.push({ path: args.path, line: args.line, body: args.body });
      return { data: {} };
    },
  });
  await postInlineComments(
    octokit,
    [
      { path: "a.ts", line: 1, body: "alpha" },
      { path: "b.ts", line: 2, body: "beta" },
    ],
    ctx,
    recordingLogger(),
  );
  for (const p of postedBodies) {
    assert.match(
      p.body,
      new RegExp(`<!-- boop-inline: ${p.path}:${p.line}:[0-9a-f]{16} -->`),
      `body for ${p.path}:${p.line} must carry the inline-key marker`,
    );
  }
});

test("postInlineComments skips posting when a candidate key already exists (QUB-103)", async () => {
  // Headline QUB-103 dedup path: a prior run (or sibling pod)
  // already posted a comment whose body carries the
  // `<!-- boop-inline: ... -->` marker for one of the
  // candidates. The next call must NOT post a duplicate. The
  // dedup is by (path, line, body-hash) — a comment at a
  // different body content posts normally.
  const posted = [];
  const octokit = makeFakeOctokit({
    listReviewComments: async () => {
      // Compute the same key the runner would compute for the
      // candidate `a.ts:1:"alpha"` body, and seed it into the
      // existing set. The other two candidates (`b.ts:2:"beta"`
      // and `c.ts:3:"gamma"`) are not in the existing set.
      const hashAlpha = await shaShort("alpha");
      return {
        data: [
          { id: 100, body: `old\n<!-- boop-inline: a.ts:1:${hashAlpha} -->\n` },
        ],
      };
    },
    createReviewComment: async (args) => {
      posted.push({ path: args.path, line: args.line });
      return { data: {} };
    },
  });
  const log = recordingLogger();
  await postInlineComments(
    octokit,
    [
      { path: "a.ts", line: 1, body: "alpha" },
      { path: "b.ts", line: 2, body: "beta" },
      { path: "c.ts", line: 3, body: "gamma" },
    ],
    ctx,
    log,
  );
  // Only the two non-duplicate candidates were posted.
  assert.deepEqual(posted.map((p) => p.path).sort(), ["b.ts", "c.ts"]);
  // The log mentions the skip count.
  assert.ok(
    log.out.some((l) => /skipped 1\/3 inline comments \(already posted\)/.test(l.msg)),
    "expected a skip-count log entry",
  );
  assert.ok(
    log.out.some((l) => /posted 2\/3 inline comments \(1 skipped as duplicates\)/.test(l.msg)),
    "expected the summary log to include the duplicate count",
  );
});

test("postInlineComments posts all when no existing keys match (QUB-103)", async () => {
  // No existing review comments on the PR. Every candidate
  // posts. Pins the happy path: the dedup machinery doesn't
  // accidentally skip everything.
  const posted = [];
  const octokit = makeFakeOctokit({
    listReviewComments: async () => ({ data: [] }),
    createReviewComment: async (args) => {
      posted.push({ path: args.path });
      return { data: {} };
    },
  });
  const log = recordingLogger();
  await postInlineComments(
    octokit,
    [
      { path: "a.ts", line: 1, body: "x" },
      { path: "b.ts", line: 2, body: "y" },
    ],
    ctx,
    log,
  );
  assert.equal(posted.length, 2);
  assert.ok(log.out.some((l) => /posted 2\/2 inline comments/.test(l.msg)));
  // No skip line on the all-fresh path.
  assert.ok(
    !log.out.some((l) => /skipped/.test(l.msg)),
    "no 'skipped' log on the all-fresh path",
  );
});

test("postInlineComments falls back to posting all when the listing throws 502 (QUB-103)", async () => {
  // The dedup is best-effort. A 502 on listReviewComments
  // cannot cost the run — the runner logs and posts every
  // candidate. Worst case is duplicate inline threads (the
  // receiver's CountPriorReviews / operator cleanup catches
  // them).
  const posted = [];
  const octokit = makeFakeOctokit({
    listReviewComments: async () => { throw new Error("502 Bad Gateway"); },
    createReviewComment: async (args) => {
      posted.push({ path: args.path });
      return { data: {} };
    },
  });
  const log = recordingLogger();
  await postInlineComments(
    octokit,
    [
      { path: "a.ts", line: 1, body: "x" },
      { path: "b.ts", line: 2, body: "y" },
    ],
    ctx,
    log,
  );
  assert.equal(posted.length, 2, "must post all candidates when list fails");
  assert.ok(
    log.out.some((l) => /list existing inline comments failed; proceeding without dedup/.test(l.msg)),
    "expected a 'list failed' log entry",
  );
});

test("postInlineComments treats distinct body content at the same line as distinct keys (QUB-103)", async () => {
  // The key includes a hash of the body, so two distinct
  // findings at the same (path, line) — e.g. security + style
  // — dedupe separately. Without the body hash, the runner
  // would treat them as the same comment and silently drop
  // the second.
  const posted = [];
  const octokit = makeFakeOctokit({
    listReviewComments: async () => {
      const hashA = await shaShort("finding A");
      return {
        data: [{ id: 100, body: `old\n<!-- boop-inline: a.ts:1:${hashA} -->\n` }],
      };
    },
    createReviewComment: async (args) => {
      posted.push(args.body);
      return { data: {} };
    },
  });
  await postInlineComments(
    octokit,
    [
      // Same path:line, different body — should NOT be skipped.
      { path: "a.ts", line: 1, body: "finding B (distinct content)" },
    ],
    ctx,
    recordingLogger(),
  );
  assert.equal(posted.length, 1, "distinct body content at the same line still posts");
  // The body hash is different from the existing one.
  assert.ok(/finding B/.test(posted[0]));
});

// --- direct unit tests for the QUB-103 helpers ------------------------
//
// These pin the inline-key shape and the marker format. They
// ride on the now-exported helpers in github.ts so a future
// change to the hash algorithm, separator, or marker prefix
// surfaces here rather than only as a downstream dedup miss
// (which is invisible until a retry actually fires).

test("inlineKeyForComment produces a deterministic <path>:<line>:<hash> key", () => {
  // The shape is part of the dedup contract — a future
  // change to the separator or hash length would silently
  // change which inline comments dedupe together. The regex
  // pins the visible structure.
  const k = inlineKeyForComment({ path: "src/a.ts", line: 10, body: "alpha" });
  assert.match(k, /^src\/a\.ts:10:[0-9a-f]{16}$/);
  // Determinism: same input -> same key. The runner relies
  // on this for the dedup to work across pod retries.
  const k2 = inlineKeyForComment({ path: "src/a.ts", line: 10, body: "alpha" });
  assert.equal(k, k2);
});

test("inlineKeyForComment distinguishes body content at the same (path, line)", () => {
  // Two findings at the same (path, line) with different
  // bodies (e.g. security + style) must yield distinct
  // keys. The body hash captures the content; the (path,
  // line) prefix alone is not enough.
  const a = inlineKeyForComment({ path: "src/a.ts", line: 1, body: "finding A" });
  const b = inlineKeyForComment({ path: "src/a.ts", line: 1, body: "finding B" });
  assert.notEqual(a, b, "distinct body content must yield distinct keys");
});

test("inlineKeyForComment distinguishes (path, line) at the same body content", () => {
  // Same body content at different (path, line) tuples
  // must yield distinct keys. The dedup is per-tuple, not
  // per-body alone.
  const a = inlineKeyForComment({ path: "src/a.ts", line: 1, body: "finding" });
  const b = inlineKeyForComment({ path: "src/b.ts", line: 1, body: "finding" });
  assert.notEqual(a, b, "distinct (path, line) must yield distinct keys");
});

test("inlineKeyForComment handles missing/empty body content safely", () => {
  // Defensive: a malformed `c.body` (undefined, null, "")
  // must not crash the helper. The runner receives
  // `{ path, line, body }` shapes from the narrator; a
  // future change there shouldn't be able to break the
  // dedup helper. The expected output collapses to
  // `<path>:<line>:<hash-of-empty>` so a synthetic
  // candidate with no body still gets a deterministic key.
  const k1 = inlineKeyForComment({ path: "x.ts", line: 1 });
  const k2 = inlineKeyForComment({ path: "x.ts", line: 1, body: "" });
  const k3 = inlineKeyForComment({ path: "x.ts", line: 1, body: null });
  assert.equal(k1, k2);
  assert.equal(k2, k3);
  assert.match(k1, /^x\.ts:1:[0-9a-f]{16}$/);
});

test("appendInlineKeyMarker appends the marker with a single newline separator (no trailing-newline body)", () => {
  // Body without trailing newline: separator is two newlines
  // so the marker reads as its own paragraph in the rendered
  // markdown. The marker survives the API as HTML comments.
  const out = appendInlineKeyMarker("review body", "src/a.ts:1:abc123");
  assert.equal(out, "review body\n\n<!-- boop-inline: src/a.ts:1:abc123 -->");
});

test("appendInlineKeyMarker collapses the separator to one newline when the body already ends with a newline", () => {
  // Body with trailing newline: the separator picks \n (not
  // \n\n) so the result is two newlines between content and
  // marker (one from the body, one from the separator). The
  // marker reads as its own paragraph in the rendered
  // markdown without piling up blank lines.
  const out = appendInlineKeyMarker("review body\n", "src/a.ts:1:abc123");
  assert.equal(out, "review body\n\n<!-- boop-inline: src/a.ts:1:abc123 -->");
});

test("appendInlineKeyMarker handles empty and null bodies", () => {
  // Empty body: same shape as the no-trailing-newline path
  // (an empty string doesn't end with a newline, so the
  // separator is \n\n). The marker still parses back via
  // parseInlineKey.
  const out1 = appendInlineKeyMarker("", "src/a.ts:1:abc123");
  assert.equal(out1, "\n\n<!-- boop-inline: src/a.ts:1:abc123 -->");
  // null body: same path (coerced to "" via ??).
  const out2 = appendInlineKeyMarker(null, "src/a.ts:1:abc123");
  assert.equal(out2, "\n\n<!-- boop-inline: src/a.ts:1:abc123 -->");
});

test("parseInlineKey extracts the inline key from a marked body", () => {
  // The happy path. The runner calls this on every review
  // comment when computing the existing-keys set for dedup;
  // a parse failure here would silently let duplicates slip
  // through.
  const body = "review body\n\n<!-- boop-inline: src/a.ts:10:abc123def4567890 -->\n";
  assert.equal(parseInlineKey(body), "src/a.ts:10:abc123def4567890");
});

test("parseInlineKey uses lastIndexOf to handle bodies that contain the literal marker text", () => {
  // A review body can legitimately contain the literal
  // `<!-- boop-inline:` text (e.g. quoting docs, JSON
  // samples). The runner takes the *trailing* marker — the
  // one this run appended — so the parse key matches the
  // key the runner just generated. Without lastIndexOf, an
  // earlier literal would win and the dedup would be wrong.
  const body =
    "review body\n\n" +
    "Note: see `<!-- boop-inline: doc-example -->` in docs.\n\n" +
    "<!-- boop-inline: src/a.ts:10:abc123def4567890 -->\n";
  assert.equal(
    parseInlineKey(body),
    "src/a.ts:10:abc123def4567890",
    "expected the trailing marker, not the literal-quoted earlier one",
  );
});

test("parseInlineKey returns null on missing / empty / malformed bodies", () => {
  // No marker at all -> null (the dedup treats absent as
  // not-present, not as a parse error). Empty / null bodies
  // are short-circuit null returns. A body with the open
  // marker but no close -> null (a half-written marker
  // should not be mistaken for a real one).
  assert.equal(parseInlineKey(null), null);
  assert.equal(parseInlineKey(""), null);
  assert.equal(parseInlineKey("plain body, no marker"), null);
  assert.equal(parseInlineKey("<!-- boop-inline: still-open"), null);
});

test("inlineKeyForComment + appendInlineKeyMarker + parseInlineKey round-trip", () => {
  // The end-to-end round-trip the dedup machinery depends
  // on: a key computed from (path, line, body) is appended
  // to the body, and parseInlineKey recovers the same key.
  // Any divergence between the three helpers breaks the
  // dedup silently.
  const cases = [
    { path: "src/a.ts", line: 10, body: "finding" },
    { path: "src/b.ts", line: 999, body: "x".repeat(1000) },
    { path: "weird/path with spaces.ts", line: 1, body: "" },
  ];
  for (const c of cases) {
    const key = inlineKeyForComment(c);
    const body = appendInlineKeyMarker(c.body, key);
    assert.equal(
      parseInlineKey(body),
      key,
      `round-trip mismatch for ${JSON.stringify(c)}`,
    );
  }
});

// --- listExistingInlineKeys (direct test) -----------------------------

test("listExistingInlineKeys walks pagination and aggregates the key set (QUB-103)", async () => {
  // Headline reviewer concern: the cross-page
  // `listReviewComments` pagination shape was tested
  // indirectly via postInlineComments. This test pins the
  // pagination contract on the underlying helper: page 1
  // returns a full page (signals "more pages"), page 2
  // returns fewer items (signals "end of list"), and the
  // helper aggregates keys across both pages.
  //
  // The seed keys span two pages so a bug that breaks
  // after page 1 (e.g. a missing `page++`) would surface
  // as a missing key.
  const { createHash } = await import("node:crypto");
  const hashA = createHash("sha256").update("alpha").digest("hex").slice(0, 16);
  const hashB = createHash("sha256").update("beta").digest("hex").slice(0, 16);
  const hashC = createHash("sha256").update("gamma").digest("hex").slice(0, 16);
  const octokit = makeFakeOctokit({
    listReviewComments: async (args) => {
      if (args.page === 1) {
        // First page is full (100 items) — signals more
        // pages. The key for "alpha" lives here. The 99
        // fillers have no marker; the helper skips them.
        return {
          data: [
            { id: 1, body: `alpha\n<!-- boop-inline: src/a.ts:1:${hashA} -->\n` },
            ...Array.from({ length: 99 }, (_, i) => ({
              id: 100 + i,
              body: `filler-${i}\n`,
            })),
          ],
        };
      }
      // Page 2: last page (fewer items, signals end).
      // Two more keys live here.
      return {
        data: [
          { id: 200, body: `beta\n<!-- boop-inline: src/b.ts:2:${hashB} -->\n` },
          { id: 201, body: `gamma\n<!-- boop-inline: src/c.ts:3:${hashC} -->\n` },
        ],
      };
    },
  });
  const keys = await listExistingInlineKeys(octokit, ctx);
  // All three keys are present despite the pagination.
  assert.equal(keys.size, 3);
  assert.ok(keys.has(`src/a.ts:1:${hashA}`));
  assert.ok(keys.has(`src/b.ts:2:${hashB}`));
  assert.ok(keys.has(`src/c.ts:3:${hashC}`));
  // The helper actually paginated (otherwise the keys
  // beyond page 1 would be missing). The makeFakeOctokit
  // helper records each call as a { listReviewComments: args }
  // element in octokit.calls, so filter for them.
  const listCalls = octokit.calls
    .map((c) => c.listReviewComments)
    .filter(Boolean);
  assert.ok(
    listCalls.length >= 2,
    `expected at least 2 pagination calls, got ${listCalls.length}`,
  );
  assert.equal(listCalls[0].page, 1);
  assert.equal(listCalls[1].page, 2);
  // The page-2 call carried per_page=100 (consistent with
  // the per_page=100 used elsewhere in the dedup code
  // path). A future change to the page size would surface
  // here.
  assert.equal(listCalls[0].per_page, 100);
});

test("listExistingInlineKeys returns an empty set when the page returns no items", async () => {
  // Counter-test: an empty first page terminates the
  // pagination immediately. Without an early break the
  // helper would loop on a `page++` that never sees a
  // result.
  const octokit = makeFakeOctokit({
    listReviewComments: async () => ({ data: [] }),
  });
  const keys = await listExistingInlineKeys(octokit, ctx);
  assert.equal(keys.size, 0);
  const listCalls = octokit.calls
    .map((c) => c.listReviewComments)
    .filter(Boolean);
  assert.equal(listCalls.length, 1);
});

test("listExistingInlineKeys dedupes duplicate keys across pages", async () => {
  // The same inline key can appear on multiple pages
  // (e.g. a prior run re-posted the same comment). The
  // Set must collapse duplicates — a future change that
  // returned an array (or a non-Set collection) would
  // surface as flakiness in the dedup math
  // (Set.has vs. array.includes).
  const { createHash } = await import("node:crypto");
  const hashA = createHash("sha256").update("alpha").digest("hex").slice(0, 16);
  const key = `src/a.ts:1:${hashA}`;
  const octokit = makeFakeOctokit({
    listReviewComments: async (args) => {
      if (args.page === 1) {
        // Full page so the loop continues. The same key
        // appears in two of the 100 items.
        const filler = Array.from({ length: 98 }, (_, i) => ({
          id: 100 + i,
          body: `filler\n`,
        }));
        return {
          data: [
            { id: 1, body: `alpha\n<!-- boop-inline: ${key} -->\n` },
            { id: 2, body: `alpha again\n<!-- boop-inline: ${key} -->\n` },
            ...filler,
          ],
        };
      }
      return { data: [] };
    },
  });
  const keys = await listExistingInlineKeys(octokit, ctx);
  assert.equal(keys.size, 1, "duplicate keys must collapse to a single set entry");
  assert.ok(keys.has(key));
});

// --- helpers for the inline-key tests ----------------------------------

// shaShort mirrors the inline-key helper in github.ts
// (sha-256 truncated to 16 hex chars). Kept local to the test
// file so a future change to the helper forces both sides to
// update together — and so the test computes the same key the
// production code does, no hard-coded literal.
async function shaShort(text) {
  const { createHash } = await import("node:crypto");
  return createHash("sha256").update(String(text)).digest("hex").slice(0, 16);
}

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

// --- renderInitialStatusBody (QUB-99) ---------------------------------

test("renderInitialStatusBody matches the receiver template", () => {
  // The receiver's pre-QUB-99 postStatus used this exact
  // body shape. The runner takes over creation on its first
  // PATCH and must produce the same surface so the user
  // experience is unchanged. The header is pinned so a
  // future rename of the "Boop's on the case" template
  // surfaces here (QUB-93 user-visible surface).
  const PAW = String.fromCodePoint(0x1f43e);
  const body = renderInitialStatusBody(ctx, {});
  assert.ok(body.startsWith(PAW + " **Boop's on the case!** (review)"),
    "initial body must start with the receiver template, got: " + body.slice(0, 60));
  assert.match(body, /Last commit: `0123456`/);
  assert.match(body, /<!-- boop-timeline -->/);
  assert.ok(!body.includes("Triggered by"));
});

test("renderInitialStatusBody renders re-review label", () => {
  const PAW = String.fromCodePoint(0x1f43e);
  const body = renderInitialStatusBody({ ...ctx, reviewNumber: 3 }, {});
  assert.ok(body.startsWith(PAW + " **Boop's on the case!** (re-review #3)"),
    "re-review body must carry the (re-review #N) label");
});

test("renderInitialStatusBody appends Triggered by when sender is set", () => {
  const body = renderInitialStatusBody(ctx, { by: "alice" });
  assert.match(body, /Triggered by @alice/);
  assert.match(body, /Last commit: `0123456`/);
});

// --- ensureStatusComment (QUB-99) ------------------------------------

test("ensureStatusComment is a no-op when ctx.statusCommentId is set", async () => {
  // The receiver-pre-create path: the runner should NOT
  // create a second comment when the id is already known.
  // The first-call lazy-create path is the only legitimate
  // way to populate the slot.
  const octokit = makeFakeOctokit();
  const slot = { value: 555 };
  const id = await ensureStatusComment(octokit, ctx, recordingLogger(), slot, null);
  assert.equal(id, 555);
  // No createComment call.
  assert.equal(octokit.calls.length, 0);
  // Slot unchanged.
  assert.equal(slot.value, 555);
});

test("ensureStatusComment creates a new comment when id is null", async () => {
  // makeFakeOctokit's default createComment returns id 555
  // so the test can assert against a known stable id.
  const octokit = makeFakeOctokit();
  const slot = { value: null };
  const id = await ensureStatusComment(octokit, ctx, recordingLogger(), slot, "alice");
  assert.ok(typeof id === "number" && id > 0, "should return a numeric id");
  assert.equal(slot.value, id, "slot should be populated with the new id");
  // The create call carries the initial body + the triggered-by line.
  const create = octokit.calls.find((c) => c.createComment);
  assert.ok(create, "createComment was called");
  const PAW = String.fromCodePoint(0x1f43e);
  assert.ok(
    create.createComment.body.startsWith(PAW + " **Boop's on the case!** (review)"),
    "createComment body must match the receiver template",
  );
  assert.match(create.createComment.body, /Triggered by @alice/);
});

test("ensureStatusComment reuses the slot on a second call", async () => {
  // The slot is a stable handle: the second lazy-create
  // (after the first PATCHed the comment) must NOT post a
  // second initial comment. Without this, a pipeline retry
  // would double-post the header.
  const octokit = makeFakeOctokit();
  const slot = { value: null };
  const id1 = await ensureStatusComment(octokit, ctx, recordingLogger(), slot, null);
  const id2 = await ensureStatusComment(octokit, ctx, recordingLogger(), slot, null);
  assert.equal(id1, id2);
  // Only one createComment call.
  const creates = octokit.calls.filter((c) => c.createComment);
  assert.equal(creates.length, 1, "ensureStatusComment should create at most one comment per slot");
});

test("ensureStatusComment returns null when no octokit is supplied", async () => {
  // The first postStatus can land before the handshake
  // mints the installation token (the octokit slot is null).
  // In that case the lazy-create is a no-op and postStatus
  // itself logs "skip" — no crash.
  const slot = { value: null };
  const id = await ensureStatusComment(null, ctx, recordingLogger(), slot, null);
  assert.equal(id, null);
  assert.equal(slot.value, null);
});

test("ensureStatusComment swallows create errors (best-effort)", async () => {
  const octokit = makeFakeOctokit({
    createComment: async () => { throw new Error("rate-limited"); },
  });
  const log = recordingLogger();
  const slot = { value: null };
  const id = await ensureStatusComment(octokit, ctx, log, slot, null);
  assert.equal(id, null);
  // Slot stays null so the runner can retry on the next PATCH.
  assert.equal(slot.value, null);
  assert.ok(
    log.out.some(
      (e) => e.level === "ERROR" && /rate-limited/.test(e.err || ""),
    ),
  );
});
