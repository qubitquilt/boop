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
// The audit split moves the markers out of github.ts so
// a future change to dedup (new marker family, hash
// algorithm swap, new QUB-XXX dedup rule) is one focused
// file instead of one file with 10 unrelated concerns.
//
// The constants here are imported by `comments.ts`
// (postReview embeds the head-SHA + review-id markers in
// the summary body) and by `auth.ts` (cleanupPriorReview
// doesn't read markers, but the post-inline path uses
// the inline-key marker). The marker helpers stay here
// because every one of them is about "what is / isn't
// a duplicate"; that's the audit's "inline-comment-dedup
// concern" grouping.

import { createHash } from "node:crypto";
import type { Ctx, Deps, OctokitLike } from "../../types.ts";

export const WORKFLOW_STATE_MARKER = "<!-- boop-state:";

export const REVIEW_ID_MARKER_PREFIX = "<!-- boop-review-id:";
export const INLINE_KEY_MARKER_PREFIX = "<!-- boop-inline:";
export const HEAD_SHA_MARKER_PREFIX = "<!-- boop-head-sha:";

export const HTML_COMMENT_CLOSE = "-->";

export async function readWorkflowState(
  octokit: OctokitLike | null,
  ctx: Ctx,
  deps: Pick<Deps, "log" | "errlog">,
): Promise<{ passed: string[]; sub: Record<string, string[]> }> {
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
      err: String(err instanceof Error ? err.message : err),
    });
    return { passed: [], sub: {} };
  }
}

function parseStateFromBody(body: string): { passed: string[]; sub: Record<string, string[]> } {
  const start = body.indexOf(WORKFLOW_STATE_MARKER);
  if (start < 0) return { passed: [], sub: {} };
  const openEnd = start + WORKFLOW_STATE_MARKER.length;
  const closeStart = body.indexOf(HTML_COMMENT_CLOSE, openEnd);
  if (closeStart < 0) return { passed: [], sub: {} };
  const json = body.slice(openEnd, closeStart).trim();
  let parsed: { passed?: unknown; sub?: unknown };
  try {
    parsed = JSON.parse(json);
  } catch {
    return { passed: [], sub: {} };
  }
  return {
    passed: Array.isArray(parsed.passed) ? (parsed.passed as string[]) : [],
    sub: parsed.sub && typeof parsed.sub === "object" ? (parsed.sub as Record<string, string[]>) : {},
  };
}

export async function writeWorkflowState(
  octokit: OctokitLike | null,
  ctx: Ctx,
  deps: Pick<Deps, "log" | "errlog">,
  state: { passed?: string[]; sub?: Record<string, string[]> },
): Promise<void> {
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
      err: String(err instanceof Error ? err.message : err),
    });
  }
}

function upsertStateInBody(body: string, line: string): string {
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

export function inlineKeyForComment(c: { body?: string; path?: string; line?: number } | null | undefined): string {
  const hash = createHash("sha256")
    .update(String(c?.body ?? ""))
    .digest("hex")
    .slice(0, 16);
  return `${c?.path}:${c?.line}:${hash}`;
}

export function appendInlineKeyMarker(body: string, key: string): string {
  const sep = String(body ?? "").endsWith("\n") ? "\n" : "\n\n";
  return `${body ?? ""}${sep}${INLINE_KEY_MARKER_PREFIX} ${key} ${HTML_COMMENT_CLOSE}`;
}

export function parseInlineKey(body: string | null | undefined): string | null {
  if (!body) return null;
  const start = body.lastIndexOf(INLINE_KEY_MARKER_PREFIX);
  if (start < 0) return null;
  const openEnd = start + INLINE_KEY_MARKER_PREFIX.length + 1;
  const closeStart = body.indexOf(HTML_COMMENT_CLOSE, openEnd);
  if (closeStart < 0) return null;
  return body.slice(openEnd, closeStart).trim();
}

export async function findExistingSummaryCommentID(
  octokit: OctokitLike,
  ctx: Ctx,
): Promise<number | null> {
  const expectedReviewIdMarker = ctx.reviewId
    ? `${REVIEW_ID_MARKER_PREFIX} ${ctx.reviewId} ${HTML_COMMENT_CLOSE}`
    : null;
  const expectedHeadShaMarker = ctx.prHeadSha
    ? `${HEAD_SHA_MARKER_PREFIX} ${ctx.prHeadSha} ${HTML_COMMENT_CLOSE}`
    : null;
  if (!expectedReviewIdMarker && !expectedHeadShaMarker) return null;
  let page = 1;
  let latest: { id: number; _index: number } | null = null;
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

export async function listExistingInlineKeys(
  octokit: OctokitLike,
  ctx: Ctx,
): Promise<Set<string>> {
  const keys = new Set<string>();
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
