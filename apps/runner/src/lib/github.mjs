// GitHub API surface.
//
// Everything the runner does against GitHub lives here:
//   - mintInstallationToken: exchange an App JWT for an installation token
//   - postStatus / postReview / postInlineComment: the public comments
//   - cleanupPriorReview: resolve outdated threads + minimize prior bot comments
//   - readWorkflowState / writeWorkflowState: the resume handoff
//     (QUB-92) — the state lives in the status comment
//   - graphql + paginated fetches used by cleanup
//
// Each function takes the loaded `ctx` and a `deps` bundle (fetch,
// Octokit factory, logger, status constants) so a test can pass a
// stub Octokit and a recording fetch without touching the network.

import { createHash } from "node:crypto";
import { Octokit } from "@octokit/rest";
import { SHORT, STATUS } from "./config.mjs";
import { shortSha } from "./security.mjs";
import { reviewHeader } from "../review-header.mjs";

// Re-export STATUS and SHORT so the test suite (workflow.test.mjs)
// can pin the user-visible surface (QUB-93) without reaching
// into config.mjs. The runner is the canonical consumer of
// these maps; the receiver mirrors them in
// apps/receiver/internal/webhook/handler.go.
export { STATUS, SHORT };

// mintInstallationToken exchanges an App JWT for an installation
// token (1h TTL). Used by both status updates and the cleanup
// GraphQL fetches.
export async function mintInstallationToken(appId, privateKey, installationId, deps) {
  const { jwt, fetch, fetchImpl = fetch, log } = deps;
  const now = Math.floor(Date.now() / 1000);
  const appJwt = jwt.sign(
    { iat: now - 30, exp: now + 600, iss: String(appId) },
    privateKey,
    { algorithm: "RS256" },
  );

  const res = await fetchImpl(
    `https://api.github.com/app/installations/${installationId}/access_tokens`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${appJwt}`,
        Accept: "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "boop-runner",
      },
    },
  );
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`mint installation token: ${res.status} ${text}`);
  }
  const data = await res.json();
  return data.token;
}

// graphql POSTs a query/mutation to api.github.com/graphql with the
// installation token and returns the JSON `data` object. Throws on
// transport errors or top-level GraphQL errors.
async function graphql(token, query, variables, deps) {
  const { fetchImpl = fetch } = deps;
  const res = await fetchImpl("https://api.github.com/graphql", {
    method: "POST",
    headers: {
      Authorization: `bearer ${token}`,
      "Content-Type": "application/json",
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": "2022-11-28",
      "User-Agent": "boop-runner",
    },
    body: JSON.stringify({ query, variables }),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`graphql HTTP ${res.status}: ${text.slice(0, 200)}`);
  }
  const json = await res.json();
  if (json.errors && json.errors.length > 0) {
    throw new Error(`graphql: ${json.errors.map((e) => e.message).join("; ")}`);
  }
  return json.data;
}

// paginateThreads walks every review thread on the PR, paginating
// until exhausted. Each returned thread is annotated with the
// original comment's author login (case-insensitive match).
async function fetchAllReviewThreads(token, ctx, deps) {
  const threads = [];
  let cursor = null;
  const query = `
    query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
      repository(owner: $owner, name: $repo) {
        pullRequest(number: $number) {
          reviewThreads(first: 100, after: $cursor) {
            pageInfo { hasNextPage endCursor }
            nodes {
              id
              isResolved
              isOutdated
              comments(first: 1) {
                nodes { author { login } }
              }
            }
          }
        }
      }
    }`;
  while (true) {
    const data = await graphql(token, query, {
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      number: Number(ctx.prNumber),
      cursor,
    }, deps);
    const conn = data?.repository?.pullRequest?.reviewThreads;
    if (!conn) break;
    for (const node of conn.nodes) {
      const author =
        node?.comments?.nodes?.[0]?.author?.login?.toLowerCase() || "";
      threads.push({
        id: node.id,
        isResolved: node.isResolved === true,
        isOutdated: node.isOutdated === true,
        author,
      });
    }
    if (!conn.pageInfo?.hasNextPage) break;
    cursor = conn.pageInfo.endCursor;
  }
  return threads;
}

// fetchPriorBotIssueCommentIDs walks every issue comment on the PR
// via the REST API (GraphQL's pullRequest.comments misses some bot
// comments that were posted via the issue-comments API). Returns
// the integer IDs of every comment posted by the bot, excluding the
// current run's status comment.
async function fetchPriorBotIssueCommentIDs(token, ctx, deps) {
  const ids = [];
  const headers = {
    Authorization: `bearer ${token}`,
    Accept: "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "boop-runner",
  };
  const { fetchImpl = fetch } = deps;

  let page = 1;
  while (true) {
    const res = await fetchImpl(
      `https://api.github.com/repos/${ctx.prOwner}/${ctx.prRepo}/issues/${ctx.prNumber}/comments?per_page=100&page=${page}`,
      { headers },
    );
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`list comments HTTP ${res.status}: ${text.slice(0, 200)}`);
    }
    const arr = await res.json();
    if (!Array.isArray(arr) || arr.length === 0) break;
    for (const c of arr) {
      const author = c?.user?.login?.toLowerCase() || "";
      if (ctx.botLogin && author !== ctx.botLogin.toLowerCase()) continue;
      if (ctx.statusCommentId && Number(c.id) === ctx.statusCommentId) continue;
      ids.push({ id: Number(c.id), nodeId: c.node_id });
    }
    if (arr.length < 100) break;
    page++;
  }
  return ids;
}

// resolveReviewThread marks one review thread as resolved.
async function resolveReviewThread(token, threadId, deps) {
  const data = await graphql(
    token,
    `mutation($id: ID!) {
       resolveReviewThread(input: { threadId: $id }) {
         thread { id isResolved }
       }
     }`,
    { id: threadId },
    deps,
  );
  return data?.resolveReviewThread?.thread?.isResolved === true;
}

// minimizeComment collapses a comment in the PR UI. The body stays
// in the API (so the boop-head-sha marker remains parsable by the
// receiver's CountPriorReviews) but the comment is hidden by default.
async function minimizeComment(token, commentNodeId, deps) {
  const data = await graphql(
    token,
    `mutation($id: ID!) {
       minimizeComment(input: { subjectId: $id, classifier: OUTDATED }) {
         minimizedComment { isMinimized }
       }
     }`,
    { id: commentNodeId },
    deps,
  );
  return data?.minimizeComment?.minimizedComment?.isMinimized === true;
}

// postStatus PATCHes the status comment with the latest stage.
// See the long comment below for the QUB-99 ordering.
//
// QUB-99: postStatus was previously bound to a receiver-pre-created
// comment. The receiver now (a) creates the durable K8s Job before
// any user-visible side effect and (b) does NOT pre-create the
// status comment. The runner is therefore responsible for creating
// the initial status comment on its first PATCH when
// ctx.statusCommentId is missing.
//
// The caller (makeDeps in index.mjs) wraps deps.postStatus with a
// `ensureStatusComment` step that lazy-creates the initial comment
// on first call and patches the slot for subsequent calls. This
// function trusts that the id is present and skips when it is not
// (same best-effort behaviour as before — no comment, no crash).
export async function postStatus(stage, detail, ctx, deps) {
  const { octokit, log, errlog } = deps;
  if (!octokit || !ctx.statusCommentId) {
    log("status", "skip (no client or comment id)", { stage });
    return;
  }
  const tpl = STATUS[stage] || `boop status: ${stage}`;
  const short = SHORT[stage] || stage;
  const entry = detail
    ? `- ${short}
  <details><summary>details</summary>

  \`\`\`
  ${detail}
  \`\`\`
  </details>`
    : `- ${short}`;
  try {
    const { data: current } = await octokit.rest.issues.getComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: ctx.statusCommentId,
    });
    const sep = "<!-- boop-timeline -->";
    let body;
    if (current.body.includes(sep)) {
      body = current.body + "\n" + entry;
    } else {
      const header = tpl.replace(/\{sha\}/g, shortSha(ctx.prHeadSha));
      body = `${header}\n\n${sep}\n${entry}`;
    }
    if (body.length > 60000) {
      const cutAt = body.indexOf(sep) + sep.length + 2;
      body = body.slice(0, cutAt) + "_(earlier entries trimmed)_\n" + body.slice(body.length - 58000);
    }
    await octokit.rest.issues.updateComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: ctx.statusCommentId,
      body,
    });
    log("status", "comment updated", { stage, comment_id: ctx.statusCommentId });
  } catch (err) {
    errlog("status", "comment update failed", { stage, err: String(err?.message ?? err) });
  }
}

// renderInitialStatusBody produces the body of the runner-created
// initial status comment. The receiver's pre-QUB-99 postStatus call
// did this; the runner now owns the role. The shape matches what the
// receiver used to render via renderStatusBody(StatusInitial, ...):
//
//	🐾 **Boop's on the case!** (review)
//	[Triggered by @user]
//	Last commit: `xxx`. Digging in now — updates will appear here.
//
//	<!-- boop-timeline -->
//
// The trailing HTML comment is the timeline separator the runner's
// PATCH path appends after (see postStatus). Without it the first
// PATCH would rewrite the receiver-supplied header instead of
// appending to the timeline. Today (2026-08-05) the only caller
// changing the header is the runner's own lazy-create — the body
// here is the only header ever written — but the separator stays
// so the format stays a strict superset of the old receiver template.
//
// `by` is the issue_comment sender login (forwarded by the receiver
// as BOOP_SENDER_LOGIN). Empty for pull_request-driven runs — those
// have no trigger attribution.
export function renderInitialStatusBody(ctx, opts = {}) {
  const short = shortSha(ctx.prHeadSha);
  const label = ctx.reviewNumber > 1 ? `re-review #${ctx.reviewNumber}` : "review";
  const byLine = opts.by ? `Triggered by @${opts.by}\n\n` : "";
  return `🐾 **Boop's on the case!** (${label})\n\n${byLine}Last commit: \`${short}\`. Digging in now — updates will appear here.\n\n<!-- boop-timeline -->`;
}

// ensureStatusComment creates the initial status comment when
// ctx.statusCommentId is unset, and caches the new id on a shared
// `slot` object so the rest of the run PATCHes the same comment.
//
// QUB-99: the receiver no longer pre-creates the status comment
// (the prior ordering left orphans when the receiver died between
// postStatus and createJob). The runner now takes over the
// creation. The first postStatus call from any stage lazily
// creates the comment and continues in PATCH mode for the rest of
// the run.
//
// Returns the comment id (existing or just-created). Returns null
// when the lazy-create could not run (no octokit) — postStatus
// then skips as before.
//
// `slot` is a mutable { value } object the caller passes so the
// caller can observe the new id (and so subsequent calls don't
// repeat the create on PATCH retries). The `by` parameter is
// forwarded from BOOP_SENDER_LOGIN into the initial body.
export async function ensureStatusComment(octokit, ctx, deps, slot, by) {
  if (slot && slot.value) return slot.value;
  if (!octokit) return null;
  const { log, errlog } = deps;
  const body = renderInitialStatusBody(ctx, { by });
  try {
    const { data: created } = await octokit.rest.issues.createComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      issue_number: Number(ctx.prNumber),
      body,
    });
    if (slot) slot.value = created.id;
    log("status", "created initial status comment", {
      comment_id: created.id,
    });
    return created.id;
  } catch (err) {
    errlog("status", "create initial status comment failed", {
      err: String(err?.message ?? err),
    });
    return null;
  }
}

// --- workflow state persistence (QUB-92) -------------------------------
//
// The runner persists its progress to the GitHub status comment so
// a failed run can resume from the failed stage. The state lives in
// a hidden HTML comment the runner reads on startup and PATCHes
// after each macro stage passes. The format is intentionally tiny
// (~80 bytes); the existing 60 KB comment trim path handles the
// rest of the body.

// WORKFLOW_STATE_MARKER is the HTML-comment delimiter the runner
// uses to find its state line in the status comment. Anything
// between the open and close tags is JSON. A runner that sees
// a missing or malformed marker treats the run as fresh.
export const WORKFLOW_STATE_MARKER = "<!-- boop-state:";

// readWorkflowState reads the status comment and returns the
// passed-macro-stages list. Best-effort: a missing comment,
// missing marker, or malformed JSON returns an empty list
// (treat as fresh run). The caller (run() in index.mjs) uses
// the list to skip already-passed stages.
export async function readWorkflowState(octokit, ctx, deps) {
  if (!octokit || !ctx.statusCommentId) {
    return { passed: [], sub: {} };
  }
  try {
    const { data: current } = await octokit.rest.issues.getComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: ctx.statusCommentId,
    });
    return parseStateFromBody(current.body || "");
  } catch (err) {
    deps.log("state", "read failed; treating as fresh", {
      err: String(err?.message ?? err),
    });
    return { passed: [], sub: {} };
  }
}

function parseStateFromBody(body) {
  const start = body.indexOf(WORKFLOW_STATE_MARKER);
  if (start < 0) return { passed: [], sub: {} };
  const openEnd = start + WORKFLOW_STATE_MARKER.length;
  const closeStart = body.indexOf("-->", openEnd);
  if (closeStart < 0) return { passed: [], sub: {} };
  const json = body.slice(openEnd, closeStart).trim();
  let parsed;
  try {
    parsed = JSON.parse(json);
  } catch {
    return { passed: [], sub: {} };
  }
  return {
    passed: Array.isArray(parsed.passed) ? parsed.passed : [],
    sub: parsed.sub && typeof parsed.sub === "object" ? parsed.sub : {},
  };
}

// writeWorkflowState PATCHes the status comment with the
// current state. The state is appended after the
// `<!-- boop-state:` marker (replacing any previous state
// line). The rest of the comment body is preserved.
//
// Best-effort: a write failure is logged but the run continues.
// The state is best-effort: a PATCH that fails leaves the
// in-memory state ahead of the comment; the next stage PATCH
// or a re-trigger's state-read will see the slightly stale
// state. The status-comment timeline is the fallback: any
// stage whose side effect (summary post, inlines post) is
// still in GitHub counts as "passed".
export async function writeWorkflowState(octokit, ctx, deps, state) {
  if (!octokit || !ctx.statusCommentId) {
    return;
  }
  const line = `${WORKFLOW_STATE_MARKER} ${JSON.stringify({
    passed: state.passed || [],
    sub: state.sub || {},
  })} -->`;
  try {
    const { data: current } = await octokit.rest.issues.getComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: ctx.statusCommentId,
    });
    const body = upsertStateInBody(current.body || "", line);
    await octokit.rest.issues.updateComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: ctx.statusCommentId,
      body,
    });
    deps.log("state", "comment updated", { passed: state.passed });
  } catch (err) {
    deps.errlog("state", "comment update failed", {
      err: String(err?.message ?? err),
    });
  }
}

function upsertStateInBody(body, line) {
  const start = body.indexOf(WORKFLOW_STATE_MARKER);
  if (start < 0) {
    return body + "\n" + line;
  }
  const closeStart = body.indexOf("-->", start);
  if (closeStart < 0) {
    return body + "\n" + line;
  }
  return body.slice(0, start) + line + body.slice(closeStart + 3);
}

// --- review-id + inline-key idempotency (QUB-103) ----------------------
//
// The runner's externally-visible writes (summary comment, inline
// threads, prior-thread cleanup) must be idempotent under pod kill.
// The workflow-state write (QUB-92) is best-effort; the real
// correctness mechanism lives in the side-effect dedup. Each
// summary comment carries a per-run `boop-review-id: <uuid>`
// marker generated at run() start. Each inline thread carries a
// per-inline `boop-inline: <path>:<line>:<body-hash>` marker. On
// retry, postReview / postInlineComments list existing artifacts
// and skip / re-PATCH instead of double-posting.
//
// The head-SHA marker (`boop-head-sha`) is the pre-QUB-103 dedup
// key. We still write it on every summary, and postReview accepts
// it as a fallback match when no review-id marker is present (so
// older summaries — written before this PR — still dedupe).

// REVIEW_ID_MARKER_PREFIX / INLINE_KEY_MARKER_PREFIX are the HTML
// comment delimiters the runner uses to find review + inline
// markers. Hidden in the rendered markdown; preserved by the
// GitHub API verbatim.
export const REVIEW_ID_MARKER_PREFIX = "<!-- boop-review-id:";
export const INLINE_KEY_MARKER_PREFIX = "<!-- boop-inline:";
export const HEAD_SHA_MARKER_PREFIX = "<!-- boop-head-sha:";
const HTML_COMMENT_CLOSE = "-->";

// inlineKeyForComment computes the deterministic per-inline key
// from (path, line, body). The body hash is sha-256 truncated to
// 16 hex chars (64 bits) — more than enough to avoid collisions
// and short enough to keep the inline marker terse. The key
// shape is `<path>:<line>:<hash>`; the body hash captures the
// actual content so two distinct findings at the same line
// (e.g. security + style) still get distinct keys.
//
// Exported so direct unit tests (github.test.mjs) can pin the
// key shape without going through the integration path. A
// future change to the hash algorithm or separator that's
// detected by these tests will surface as a clear contract
// violation rather than as a downstream dedup-miss.
export function inlineKeyForComment(c) {
  const hash = createHash("sha256")
    .update(String(c?.body ?? ""))
    .digest("hex")
    .slice(0, 16);
  return `${c?.path}:${c?.line}:${hash}`;
}

// appendInlineKeyMarker appends the deterministic key marker to
// a comment body. Hidden in the rendered markdown. Always called
// before posting (the marker survives the GitHub API as-is), so
// a re-run's listExistingInlineKeys can detect duplicates even
// when the workflow-state marker (QUB-92) is stale or missing.
//
// Exported for direct unit tests — see parseInlineKey below.
export function appendInlineKeyMarker(body, key) {
  const sep = String(body ?? "").endsWith("\n") ? "\n" : "\n\n";
  return `${body ?? ""}${sep}${INLINE_KEY_MARKER_PREFIX} ${key} ${HTML_COMMENT_CLOSE}`;
}

// parseInlineKey extracts the inline key from a comment body,
// or returns null if no marker is present. The runner calls this
// on every review comment when computing the existing-keys set
// for dedup. Uses `lastIndexOf` so a body that legitimately
// contains the literal `<!-- boop-inline:` text (e.g., quoting
// docs, JSON keys, etc.) is parsed correctly — only the trailing
// marker counts.
//
// Exported for direct unit tests.
export function parseInlineKey(body) {
  if (!body) return null;
  const start = body.lastIndexOf(INLINE_KEY_MARKER_PREFIX);
  if (start < 0) return null;
  const openEnd = start + INLINE_KEY_MARKER_PREFIX.length + 1;
  const closeStart = body.indexOf("-->", openEnd);
  if (closeStart < 0) return null;
  return body.slice(openEnd, closeStart).trim();
}

// findExistingSummaryCommentID walks every issue comment on the
// PR (paginated) and returns the integer id of the most recent
// one whose body matches the review-id or head-SHA marker. Used
// by postReview to PATCH instead of POST on retry. The dedup is
// best-effort: if the listing throws (502 / timeout), the
// caller's try/catch logs and falls back to a fresh POST so a
// transient listing error doesn't cost the run.
async function findExistingSummaryCommentID(octokit, ctx) {
  const expectedReviewIdMarker = ctx.reviewId
    ? `${REVIEW_ID_MARKER_PREFIX} ${ctx.reviewId} ${HTML_COMMENT_CLOSE}`
    : null;
  const expectedHeadShaMarker = ctx.prHeadSha
    ? `${HEAD_SHA_MARKER_PREFIX} ${ctx.prHeadSha} ${HTML_COMMENT_CLOSE}`
    : null;
  if (!expectedReviewIdMarker && !expectedHeadShaMarker) return null;
  let page = 1;
  let latest = null;
  let absoluteIndex = 0;
  while (true) {
    const { data: arr } = await octokit.rest.issues.listComments({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      issue_number: Number(ctx.prNumber),
      per_page: 100,
      page,
    });
    if (!Array.isArray(arr) || arr.length === 0) break;
    for (const c of arr) {
      const body = c.body || "";
      const matches =
        (expectedReviewIdMarker && body.includes(expectedReviewIdMarker)) ||
        (expectedHeadShaMarker && body.includes(expectedHeadShaMarker));
      if (matches) {
        const candidate = { id: c.id, _index: absoluteIndex };
        if (!latest || candidate._index > latest._index) {
          latest = candidate;
        }
      }
      absoluteIndex++;
    }
    if (arr.length < 100) break;
    page++;
  }
  return latest ? latest.id : null;
}

// listExistingInlineKeys walks every PR review comment
// (paginated) and returns the set of inline-key markers found
// across their bodies. Used by postInlineComments to filter out
// duplicates on retry. A failed list is treated as an empty set
// (the caller logs and proceeds) — best-effort dedup, not a
// correctness gate.
//
// Exported so direct unit tests can pin the pagination shape
// (per_page, last-page break) without going through
// postInlineComments.
export async function listExistingInlineKeys(octokit, ctx) {
  const keys = new Set();
  let page = 1;
  while (true) {
    const { data: arr } = await octokit.rest.pulls.listReviewComments({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      pull_number: Number(ctx.prNumber),
      per_page: 100,
      page,
    });
    if (!Array.isArray(arr) || arr.length === 0) break;
    for (const c of arr) {
      const k = parseInlineKey(c.body || "");
      if (k) keys.add(k);
    }
    if (arr.length < 100) break;
    page++;
  }
  return keys;
}

// makeOctokit creates an Octokit bound to the installation token.
// Exported so callers can reuse the same instance for both REST and
// the postStatus / postReview / postInlineComment calls.
export function makeOctokit(installationToken, deps = {}) {
  const OctokitCtor = deps.OctokitCtor || Octokit;
  return new OctokitCtor({ auth: installationToken });
}

// postReview creates the summary comment with the confidence badge,
// review header, and the hidden head-SHA + per-run review-id markers
// for re-review diffing and QUB-103 idempotency.
//
// QUB-103: the runner PATCHes an existing summary when one already
// carries the matching review-id or head-SHA marker. The fallback
// to head-SHA dedup keeps older summaries (pre-QUB-103) covered.
// `deps` is optional; passing `{ log }` lets the caller observe
// which side-effect path fired (created vs patched). Throwing on a
// list failure would surface to the orchestrator catch in index.mjs
// and force the run to abort — we instead swallow and fall back to
// a fresh POST so a transient listing error doesn't cost the run.
export async function postReview(octokit, body, reviewNumber, confidence, ctx, deps = {}) {
  const max = 65000;
  const cleaned = body.replace(/\n{3,}/g, "\n\n").trim();
  const trimmed = cleaned.length > max ? cleaned.slice(0, max - 50) + "\n\n…(truncated)" : cleaned;
  const reviewTag = reviewNumber > 1 ? ` · review #${reviewNumber}` : "";
  const badge = confidenceBadgeLocal(confidence || "medium");
  // Hidden marker carrying the full head SHA so the next re-review
  // can diff the delta from this commit. The receiver parses this
  // (see priorReviewHeadSHARegex in client.go). GitHub renders HTML
  // comments as nothing in the markdown view, so it's invisible to
  // human readers.
  const headMarker = `${HEAD_SHA_MARKER_PREFIX} ${ctx.prHeadSha} ${HTML_COMMENT_CLOSE}`;
  // QUB-103: per-run review id lets the runner find the existing
  // summary on retry and PATCH it instead of double-posting. The
  // UUID is generated at run() start in index.mjs and threaded
  // through ctx; tests inject it via fixture ctx.
  const reviewIdMarker = ctx.reviewId
    ? `\n${REVIEW_ID_MARKER_PREFIX} ${ctx.reviewId} ${HTML_COMMENT_CLOSE}`
    : "";
  const fullBody =
    `${reviewHeader(reviewNumber)}\n\n` +
    `${badge}\n\n` +
    trimmed +
    `\n\n<sub>Posted by [BoopPr](https://github.com/qubitquilt/boop) · PR \`${shortSha(ctx.prHeadSha)}\`${reviewTag} · good boy powered</sub>` +
    `\n${headMarker}` +
    reviewIdMarker;

  let existingId = null;
  try {
    existingId = await findExistingSummaryCommentID(octokit, ctx);
  } catch (err) {
    deps.log?.("review", "list existing summary comments failed; falling back to create", {
      err: String(err?.message ?? err),
    });
  }
  if (existingId) {
    await octokit.rest.issues.updateComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: existingId,
      body: fullBody,
    });
    deps.log?.("review", "patched existing summary comment", {
      comment_id: existingId,
      review_id: ctx.reviewId || null,
    });
    return;
  }
  await octokit.rest.issues.createComment({
    owner: ctx.prOwner,
    repo: ctx.prRepo,
    issue_number: Number(ctx.prNumber),
    body: fullBody,
  });
  deps.log?.("review", "created summary comment", {
    review_id: ctx.reviewId || null,
  });
}

// postInlineComment creates a single line-specific review comment on
// the PR. Uses GitHub's "review comments" API which renders inline on
// the file diff.
async function postInlineComment(octokit, c, ctx) {
  await octokit.rest.pulls.createReviewComment({
    owner: ctx.prOwner,
    repo: ctx.prRepo,
    pull_number: Number(ctx.prNumber),
    body: c.body,
    commit_id: ctx.prHeadSha,
    path: c.path,
    line: c.line,
    side: "RIGHT",
  });
}

// postInlineComments posts every line-specific comment in parallel.
// Each comment is independent, so a single failure does not block the
// rest. We use Promise.allSettled so failures are surfaced via the
// logger rather than thrown.
//
// QUB-103: every candidate inline is enriched with a deterministic
// `<!-- boop-inline: <path>:<line>:<body-hash> -->` marker before
// posting. On entry, the runner lists the PR's existing review
// comments, parses their markers, and skips any candidate whose key
// is already present. This decouples inline dedup from the
// fire-and-forget workflow-state write (QUB-92) — a pod kill that
// posted half the inlines before dying cannot cause the next pod to
// re-post them.
export async function postInlineComments(octokit, comments, ctx, deps) {
  const { log, errlog } = deps;
  let existingKeys = new Set();
  if (comments.length > 0) {
    try {
      existingKeys = await listExistingInlineKeys(octokit, ctx);
    } catch (err) {
      errlog("inline", "list existing inline comments failed; proceeding without dedup", {
        err: String(err?.message ?? err),
      });
    }
  }

  const enriched = comments.map((c) => {
    const key = inlineKeyForComment(c);
    return { c, key, body: appendInlineKeyMarker(c.body, key) };
  });

  const toPost = enriched.filter(({ key }) => !existingKeys.has(key));
  const skipped = comments.length - toPost.length;
  if (skipped > 0) {
    log("inline", `skipped ${skipped}/${comments.length} inline comments (already posted)`, {
      skipped,
    });
  }

  const results = await Promise.allSettled(
    toPost.map(({ body, c }) => postInlineComment(octokit, { ...c, body }, ctx)),
  );
  let ok = 0;
  results.forEach((r, i) => {
    if (r.status === "fulfilled") {
      ok++;
    } else {
      const c = toPost[i].c;
      errlog("inline", "failed to post inline comment", {
        path: c.path,
        line: c.line,
        err: String(r.reason?.message ?? r.reason),
      });
    }
  });
  if (comments.length > 0) {
    const tail = skipped > 0 ? ` (${skipped} skipped as duplicates)` : "";
    log("done", `posted ${ok}/${comments.length} inline comments${tail}`);
  }
}

// cleanupPriorReview runs on re-reviews only. It:
//   1. Resolves every Boop review thread whose diff line is gone or
//      changed (isOutdated === true) — the author has either fixed
//      the issue or removed the code.
//   2. Minimizes every other prior Boop issue comment (status
//      threads, prior summary comments) so the PR UI is dominated
//      by the active review.
//
// Best-effort. The review already posted — a cleanup failure is
// logged but does not fail the run. The two fetches (review threads,
// issue comments) run in parallel; without that, a slow reviewThreads
// fetch would block the minimize pass by ~tens of seconds.
export async function cleanupPriorReview(token, ctx, deps) {
  const { log, errlog } = deps;
  const result = { resolved: 0, minimized: 0, errors: 0 };

  // 1+2 in parallel. Each returns a list we then iterate serially to
  // mutate the threads/comments — the network wait is the slow part
  // and that's what we're collapsing.
  const [threads, priors] = await Promise.all([
    fetchAllReviewThreads(token, ctx, deps),
    fetchPriorBotIssueCommentIDs(token, ctx, deps),
  ]);

  const targets = threads.filter(
    (t) =>
      !t.isResolved &&
      t.isOutdated &&
      ctx.botLogin &&
      t.author === ctx.botLogin.toLowerCase(),
  );
  log("cleanup", "scanned review threads", {
    total: threads.length,
    bot_outdated: targets.length,
  });
  for (const t of targets) {
    try {
      if (await resolveReviewThread(token, t.id, deps)) {
        result.resolved++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "resolve failed", {
        thread: t.id,
        err: String(err?.message ?? err),
      });
    }
  }

  log("cleanup", "scanned issue comments", { bot_total: priors.length });
  for (const c of priors) {
    try {
      if (await minimizeComment(token, c.nodeId, deps)) {
        result.minimized++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "minimize failed", {
        comment: c.id,
        err: String(err?.message ?? err),
      });
    }
  }

  return result;
}

// confidenceBadge mirrors the one in lib/opencode.mjs; inlined here
// because the comment-rendering path doesn't need the opencode deps
// and tests for postReview can import this module without pulling in
// the opencode pipeline.
function confidenceBadgeLocal(c) {
  switch (c) {
    case "high":
      return "✅ **Confidence: high** — ready to merge.";
    case "medium":
      return "⚠️ **Confidence: medium** — Follow-ups worth addressing, no Blocking findings.";
    case "low":
    default:
      return "🚨 **Confidence: low** — Blocking finding(s) present, not safe to merge without changes.";
  }
}
