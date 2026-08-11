// GitHub comment markers (RF-020 split).
//
// The runner's idempotency / dedup surface is built on hidden
// HTML-comment markers embedded in summary bodies, status
// comments, and inline review comments. Three marker
// families live here:
//
//   - WORKFLOW_STATE_MARKER     <!-- boop-state: ... -->
//       Tracks the resume state across pod restarts
//       (QUB-92). Read + written by readWorkflowState /
//       writeWorkflowState on every macro stage transition.
//
//   - REVIEW_ID_MARKER_PREFIX   <!-- boop-review-id: <uuid> -->
//     HEAD_SHA_MARKER_PREFIX    <!-- boop-head-sha: <sha> -->
//       Track the per-run summary comment and the head
//       SHA the summary was generated against (QUB-103).
//       The head-SHA fallback keeps older summaries
//       (pre-QUB-103) covered.
//
//   - INLINE_KEY_MARKER_PREFIX  <!-- boop-inline: <path>:<line>:<hash> -->
//       Per-inline dedup (also QUB-103). The body hash
//       distinguishes two findings at the same line
//       (e.g. security + style).
//
// The audit split moves the markers out of github.mjs so
// a future change to dedup (new marker family, hash
// algorithm swap, new QUB-XXX dedup rule) is one focused
// file instead of one file with 10 unrelated concerns.
//
// The constants here are imported by `comments.mjs`
// (postReview embeds the head-SHA + review-id markers in
// the summary body) and by `auth.mjs` (cleanupPriorReview
// doesn't read markers, but the post-inline path uses
// the inline-key marker). The marker helpers stay here
// because every one of them is about "what is / isn't
// a duplicate"; that's the audit's "inline-comment-dedup
// concern" grouping.

import { createHash } from "node:crypto";

// WORKFLOW_STATE_MARKER is the prefix the runner embeds
// in the status comment body to track the resume state.
// The state itself is JSON written after the marker on
// the same line; a future read parses the line by
// splitting on the marker. Exported so the test suite
// can pin the user-visible surface.
export const WORKFLOW_STATE_MARKER = "<!-- boop-state:";

// REVIEW_ID_MARKER_PREFIX / INLINE_KEY_MARKER_PREFIX are
// the HTML comment delimiters the runner uses to find
// review + inline markers. Hidden in the rendered
// markdown; preserved by the GitHub API verbatim.
export const REVIEW_ID_MARKER_PREFIX = "<!-- boop-review-id:";
export const INLINE_KEY_MARKER_PREFIX = "<!-- boop-inline:";
export const HEAD_SHA_MARKER_PREFIX = "<!-- boop-head-sha:";

// HTML_COMMENT_CLOSE is the closing delimiter for every
// marker. Lifted out so the marker constants and the
// postReview / appendInlineKeyMarker paths share one
// source of truth (the runner has no other HTML-comment
// surface).
export const HTML_COMMENT_CLOSE = "-->";

// readWorkflowState reads the status comment and returns
// the passed-macro-stages list. Best-effort: a missing
// comment, missing marker, or malformed JSON returns an
// empty list (treat as fresh run). The caller (run() in
// index.mjs) uses the list to skip already-passed
// stages.
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

// parseStateFromBody extracts the JSON payload between
// the WORKFLOW_STATE_MARKER and the HTML comment close.
// Returns the empty state when the marker is absent or
// the JSON is malformed. A runner that sees the empty
// state treats the run as fresh.
function parseStateFromBody(body) {
  const start = body.indexOf(WORKFLOW_STATE_MARKER);
  if (start < 0) return { passed: [], sub: {} };
  const openEnd = start + WORKFLOW_STATE_MARKER.length;
  const closeStart = body.indexOf(HTML_COMMENT_CLOSE, openEnd);
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
// Best-effort: a write failure is logged but the run
// continues. The state is best-effort: a PATCH that
// fails leaves the in-memory state ahead of the comment;
// the next stage PATCH or a re-trigger's state-read will
// see the slightly stale state. The status-comment
// timeline is the fallback: any stage whose side effect
// (summary post, inlines post) is still in GitHub counts
// as "passed".
export async function writeWorkflowState(octokit, ctx, deps, state) {
  if (!octokit || !ctx.statusCommentId) {
    return;
  }
  const line = `${WORKFLOW_STATE_MARKER} ${JSON.stringify({
    passed: state.passed || [],
    sub: state.sub || {},
  })} ${HTML_COMMENT_CLOSE}`;
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

// upsertStateInBody replaces any existing
// WORKFLOW_STATE_MARKER line in `body` with `line`, or
// appends `line` if no marker is present. Used by
// writeWorkflowState and the test fixture for
// writeWorkflowState.
function upsertStateInBody(body, line) {
  const start = body.indexOf(WORKFLOW_STATE_MARKER);
  if (start < 0) {
    return body + "\n" + line;
  }
  const closeStart = body.indexOf(HTML_COMMENT_CLOSE, start);
  if (closeStart < 0) {
    return body + "\n" + line;
  }
  return body.slice(0, start) + line + body.slice(closeStart + HTML_COMMENT_CLOSE.length);
}

// inlineKeyForComment computes the deterministic per-inline
// key from (path, line, body). The body hash is sha-256
// truncated to 16 hex chars (64 bits) — more than enough
// to avoid collisions and short enough to keep the inline
// marker terse. The key shape is `<path>:<line>:<hash>`;
// the body hash captures the actual content so two
// distinct findings at the same line (e.g. security +
// style) still get distinct keys.
//
// Exported so direct unit tests (github.test.mjs) can pin
// the key shape without going through the integration
// path. A future change to the hash algorithm or separator
// that's detected by these tests will surface as a clear
// contract violation rather than as a downstream
// dedup-miss.
export function inlineKeyForComment(c) {
  const hash = createHash("sha256")
    .update(String(c?.body ?? ""))
    .digest("hex")
    .slice(0, 16);
  return `${c?.path}:${c?.line}:${hash}`;
}

// appendInlineKeyMarker appends the deterministic key
// marker to a comment body. Hidden in the rendered
// markdown. Always called before posting (the marker
// survives the GitHub API as-is), so a re-run's
// listExistingInlineKeys can detect duplicates even
// when the workflow-state marker (QUB-92) is stale or
// missing.
//
// Exported for direct unit tests — see parseInlineKey
// below.
export function appendInlineKeyMarker(body, key) {
  const sep = String(body ?? "").endsWith("\n") ? "\n" : "\n\n";
  return `${body ?? ""}${sep}${INLINE_KEY_MARKER_PREFIX} ${key} ${HTML_COMMENT_CLOSE}`;
}

// parseInlineKey extracts the inline key from a comment
// body, or returns null if no marker is present. The
// runner calls this on every review comment when
// computing the existing-keys set for dedup. Uses
// `lastIndexOf` so a body that legitimately contains the
// literal `<!-- boop-inline:` text (e.g., quoting docs,
// JSON keys, etc.) is parsed correctly — only the
// trailing marker counts.
//
// Exported for direct unit tests.
export function parseInlineKey(body) {
  if (!body) return null;
  const start = body.lastIndexOf(INLINE_KEY_MARKER_PREFIX);
  if (start < 0) return null;
  const openEnd = start + INLINE_KEY_MARKER_PREFIX.length + 1;
  const closeStart = body.indexOf(HTML_COMMENT_CLOSE, openEnd);
  if (closeStart < 0) return null;
  return body.slice(openEnd, closeStart).trim();
}

// findExistingSummaryCommentID walks every issue comment
// on the PR (paginated) and returns the integer id of the
// most recent one whose body matches the review-id or
// head-SHA marker. Used by postReview to PATCH instead of
// POST on retry. The dedup is best-effort: if the listing
// throws (502 / timeout), the caller's try/catch logs and
// falls back to a fresh POST so a transient listing error
// doesn't cost the run.
export async function findExistingSummaryCommentID(octokit, ctx) {
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
// (paginated) and returns the set of inline-key markers
// found across their bodies. Used by postInlineComments
// to filter out duplicates on retry. A failed list is
// treated as an empty set (the caller logs and proceeds)
// — best-effort dedup, not a correctness gate.
//
// Exported so direct unit tests can pin the pagination
// shape (per_page, last-page break) without going through
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
