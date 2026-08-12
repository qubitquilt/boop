// GitHub comments: status thread + summary + inline
// (RF-020 split).
//
// The "visible surface" of the runner. Every function
// here produces something the PR author sees: the
// 🐾 "Boop's on the case!" status comment, the final
// summary, and the line-specific review comments. The
// dedup markers (<!-- boop-state: -->, <!-- boop-review-id:
// -->, <!-- boop-head-sha: -->, <!-- boop-inline: -->)
// live in `./markers.ts`; the auth/lifecycle surface
// (token mint, Octokit factory, cleanup) lives in
// `./auth.ts`. The audit groups this file under
// "status/comments are the visible-surface concern."
//
// postStatus + ensureStatusComment + postFinalReaction
// are the status-comment lifecycle. postReview +
// postInlineComments are the review body. renderInitial
// StatusBody is the body template used by the first
// postStatus call (so the orchestrator can pre-render
// the comment text without doing the GitHub POST).
// findExistingSummaryCommentID is the dedup entry
// point; it lives here because the comment it finds is
// the summary (the visible surface), even though the
// marker parsing is in markers.ts.

import { shortSha } from "../security.ts";
import { reviewHeader } from "../../review-header.ts";
import { SHORT, STATUS } from "../config.ts";
import {
  REVIEW_ID_MARKER_PREFIX,
  HEAD_SHA_MARKER_PREFIX,
  HTML_COMMENT_CLOSE,
  appendInlineKeyMarker,
  findExistingSummaryCommentID,
  inlineKeyForComment,
  listExistingInlineKeys,
} from "./markers.ts";
import type { Ctx, Deps, OctokitLike, Review } from "../../types.ts";

export async function postStatus(
  stage: string,
  detail: string | undefined,
  ctx: Ctx,
  deps: { octokit: OctokitLike | null; log: Deps["log"]; errlog: Deps["errlog"] },
): Promise<void> {
  const { octokit, log, errlog } = deps;
  if (ctx.noStatusComment) {
    log("status", "skip (reaction mode)", { stage });
    return;
  }
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
    let body: string;
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
    errlog("status", "comment update failed", { stage, err: String(err instanceof Error ? err.message : err) });
  }
}

export function renderInitialStatusBody(ctx: Ctx, opts: { by?: string } = {}): string {
  const short = shortSha(ctx.prHeadSha);
  const label = ctx.reviewNumber > 1 ? `re-review #${ctx.reviewNumber}` : "review";
  const byLine = opts.by ? `Triggered by @${opts.by}\n\n` : "";
  return `🐾 **Boop's on the case!** (${label})\n\n${byLine}Last commit: \`${short}\`. Digging in now — updates will appear here.\n\n<!-- boop-timeline -->`;
}

export async function ensureStatusComment(
  octokit: OctokitLike | null,
  ctx: Ctx,
  deps: { log: Deps["log"]; errlog: Deps["errlog"] },
  slot: { value: number | null } | null,
  by: string | null,
): Promise<number | null> {
  if (ctx.noStatusComment) return null;
  if (slot && slot.value) return slot.value;
  if (!octokit) return null;
  const { log, errlog } = deps;
  const body = renderInitialStatusBody(ctx, { by: by || undefined });
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
      err: String(err instanceof Error ? err.message : err),
    });
    return null;
  }
}

export async function postFinalReaction(
  stage: string,
  ctx: Ctx,
  deps: { getOctokit?: () => OctokitLike | null; octokit?: OctokitLike | null; log: Deps["log"]; errlog: Deps["errlog"] },
): Promise<void> {
  const { log, errlog } = deps;
  const octokit = deps.getOctokit ? deps.getOctokit() : (deps.octokit ?? null);
  if (!octokit || !ctx.reactionCommentId) {
    log("status", "skip final reaction (no octokit or comment id)", {
      stage,
      hasOctokit: !!octokit,
      hasReactionCommentId: !!ctx.reactionCommentId,
    });
    return;
  }
  const content =
    stage === "done"
      ? "bone"
      : stage === "failed"
        ? "x"
        : null;
  if (!content) {
    log("status", "skip final reaction (unknown stage)", { stage });
    return;
  }
  try {
    await octokit.rest.reactions.createForIssueComment({
      owner: ctx.prOwner,
      repo: ctx.prRepo,
      comment_id: Number(ctx.reactionCommentId),
      content,
    });
    log("status", "final reaction added", {
      stage,
      content,
      comment_id: ctx.reactionCommentId,
    });
  } catch (err) {
    errlog("status", "final reaction failed", {
      stage,
      content,
      err: String(err instanceof Error ? err.message : err),
    });
  }
}

export async function postReview(
  octokit: OctokitLike,
  body: string,
  reviewNumber: number,
  confidence: string,
  ctx: Ctx,
  deps: { log?: Deps["log"] } = {},
): Promise<void> {
  const max = 65000;
  const cleaned = body.replace(/\n{3,}/g, "\n\n").trim();
  const trimmed = cleaned.length > max ? cleaned.slice(0, max - 50) + "\n\n…(truncated)" : cleaned;
  const reviewTag = reviewNumber > 1 ? ` · review #${reviewNumber}` : "";
  const badge = confidenceBadgeLocal(confidence || "medium");
  const headMarker = `${HEAD_SHA_MARKER_PREFIX} ${ctx.prHeadSha} ${HTML_COMMENT_CLOSE}`;
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

  let existingId: number | null = null;
  try {
    existingId = await findExistingSummaryCommentID(octokit, ctx);
  } catch (err) {
    deps.log?.("review", "list existing summary comments failed; falling back to create", {
      err: String(err instanceof Error ? err.message : err),
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

async function postInlineComment(
  octokit: OctokitLike,
  c: { body: string; path: string; line: number },
  ctx: Ctx,
): Promise<void> {
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

export async function postInlineComments(
  octokit: OctokitLike,
  comments: Review["inlineComments"],
  ctx: Ctx,
  deps: { log: Deps["log"]; errlog: Deps["errlog"] },
): Promise<void> {
  const { log, errlog } = deps;
  let existingKeys = new Set<string>();
  if (comments.length > 0) {
    try {
      existingKeys = await listExistingInlineKeys(octokit, ctx);
    } catch (err) {
      errlog("inline", "list existing inline comments failed; proceeding without dedup", {
        err: String(err instanceof Error ? err.message : err),
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
      const c = toPost[i]?.c;
      if (!c) return;
      errlog("inline", "failed to post inline comment", {
        path: c.path,
        line: c.line,
        err: String(r.reason instanceof Error ? r.reason.message : r.reason),
      });
    }
  });
  if (comments.length > 0) {
    const tail = skipped > 0 ? ` (${skipped} skipped as duplicates)` : "";
    log("done", `posted ${ok}/${comments.length} inline comments${tail}`);
  }
}

function confidenceBadgeLocal(c: string): string {
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
