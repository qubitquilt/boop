// GitHub comments: status thread + summary + inline
// (RF-020 split).
//
// The "visible surface" of the runner. Every function
// here produces something the PR author sees: the
// 🐾 "Boop's on the case!" status comment, the final
// summary, and the line-specific review comments. The
// dedup markers (<!-- boop-state: -->, <!-- boop-review-id:
// -->, <!-- boop-head-sha: -->, <!-- boop-inline: -->)
// live in `./markers.mjs`; the auth/lifecycle surface
// (token mint, Octokit factory, cleanup) lives in
// `./auth.mjs`. The audit groups this file under
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
// marker parsing is in markers.mjs.

import { shortSha } from "../security.mjs";
import { reviewHeader } from "../../review-header.mjs";
import { SHORT, STATUS } from "../config.mjs";
import {
  REVIEW_ID_MARKER_PREFIX,
  HEAD_SHA_MARKER_PREFIX,
  HTML_COMMENT_CLOSE,
  appendInlineKeyMarker,
  findExistingSummaryCommentID,
  inlineKeyForComment,
  listExistingInlineKeys,
} from "./markers.mjs";

// postStatus PATCHes the status comment with the current
// stage line. The first call (stage=review) lazy-creates
// the comment via ensureStatusComment (QUB-99) and caches
// the id in the slot. Subsequent calls PATCH in place.
// Errors are logged but never raised: a status-comment
// blip must not abort a review.
export async function postStatus(stage, detail, ctx, deps) {
  const { octokit, log, errlog } = deps;
  // QUB-114: skip the entire status-comment path when
  // the review was triggered by an issue_comment. The
  // runner adds a single terminal reaction (see
  // postFinalReaction) on done/failed; interim stages
  // are silent.
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

// renderInitialStatusBody produces the body of the
// runner-created initial status comment. The receiver's
// pre-QUB-99 postStatus call did this; the runner now
// owns the role. The shape matches what the receiver
// used to render via renderStatusBody(StatusInitial, ...):
//
//	🐾 **Boop's on the case!** (review)
//	[Triggered by @user]
//	Last commit: `xxx`. Digging in now — updates will appear here.
//
//	<!-- boop-timeline -->
//
// The trailing HTML comment is the timeline separator
// the runner's PATCH path appends after (see postStatus).
// Without it the first PATCH would rewrite the
// receiver-supplied header instead of appending to the
// timeline. Today (2026-08-05) the only caller changing
// the header is the runner's own lazy-create — the body
// here is the only header ever written — but the
// separator stays so the format stays a strict superset
// of the old receiver template.
//
// `by` is the issue_comment sender login (forwarded by
// the receiver as BOOP_SENDER_LOGIN). Empty for
// pull_request-driven runs — those have no trigger
// attribution.
export function renderInitialStatusBody(ctx, opts = {}) {
  const short = shortSha(ctx.prHeadSha);
  const label = ctx.reviewNumber > 1 ? `re-review #${ctx.reviewNumber}` : "review";
  const byLine = opts.by ? `Triggered by @${opts.by}\n\n` : "";
  return `🐾 **Boop's on the case!** (${label})\n\n${byLine}Last commit: \`${short}\`. Digging in now — updates will appear here.\n\n<!-- boop-timeline -->`;
}

// ensureStatusComment creates the initial status comment
// when ctx.statusCommentId is unset, and caches the new
// id on a shared `slot` object so the rest of the run
// PATCHes the same comment.
//
// QUB-99: the receiver no longer pre-creates the status
// comment (the prior ordering left orphans when the
// receiver died between postStatus and createJob). The
// runner now takes over the creation. The first
// postStatus call from any stage lazily creates the
// comment and continues in PATCH mode for the rest of
// the run.
//
// Returns the comment id (existing or just-created).
// Returns null when the lazy-create could not run (no
// octokit) — postStatus then skips as before.
//
// `slot` is a mutable { value } object the caller passes
// so the caller can observe the new id (and so
// subsequent calls don't repeat the create on PATCH
// retries). The `by` parameter is forwarded from
// BOOP_SENDER_LOGIN into the initial body.
export async function ensureStatusComment(octokit, ctx, deps, slot, by) {
  if (ctx.noStatusComment) return null;
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

// postFinalReaction adds a single terminal reaction to
// the trigger comment. Used in QUB-114's reaction mode
// (no status comment): the receiver added 👀 on job
// submission, the runner adds 🦴 on done or ❌ on failed.
// One reaction, one notification, no PATCH loop.
//
// The octokit instance is resolved from the deps slot
// via `deps.getOctokit?.()` (the same pattern postStatus
// uses). Falling back to `deps.octokit` keeps test
// fixtures that inject a pre-built octokit directly.
// Without this resolution the function would always
// short-circuit on the slot-only shape that the live
// runner passes — see the review that caught this in
// `apps/runner/src/index.mjs`.
//
// `content` is the GitHub reaction emoji name (e.g.
// "eyes", "bone", "x"). GitHub's reaction API rejects
// duplicate (user, comment, content) triples with 422,
// which is the same surface we already get on a
// redelivery — so the orderly path does not need extra
// handling.
//
// `reactionCommentId` (ctx.reactionCommentId) is the
// issue_comment id the receiver threaded through.
// Empty for pull_request-driven runs — those have no
// trigger comment to react to, so postFinalReaction is
// a no-op.
export async function postFinalReaction(stage, ctx, deps) {
  const { log, errlog } = deps;
  const octokit = deps.getOctokit ? deps.getOctokit() : deps.octokit;
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
      err: String(err?.message ?? err),
    });
  }
}

// postReview creates the summary comment with the
// confidence badge, review header, and the hidden
// head-SHA + per-run review-id markers for re-review
// diffing and QUB-103 idempotency.
//
// QUB-103: the runner PATCHes an existing summary when
// one already carries the matching review-id or
// head-SHA marker. The fallback to head-SHA dedup keeps
// older summaries (pre-QUB-103) covered. `deps` is
// optional; passing `{ log }` lets the caller observe
// which side-effect path fired (created vs patched).
// Throwing on a list failure would surface to the
// orchestrator catch in index.mjs and force the run to
// abort — we instead swallow and fall back to a fresh
// POST so a transient listing error doesn't cost the
// run.
export async function postReview(octokit, body, reviewNumber, confidence, ctx, deps = {}) {
  const max = 65000;
  const cleaned = body.replace(/\n{3,}/g, "\n\n").trim();
  const trimmed = cleaned.length > max ? cleaned.slice(0, max - 50) + "\n\n…(truncated)" : cleaned;
  const reviewTag = reviewNumber > 1 ? ` · review #${reviewNumber}` : "";
  const badge = confidenceBadgeLocal(confidence || "medium");
  // Hidden marker carrying the full head SHA so the
  // next re-review can diff the delta from this commit.
  // The receiver parses this (see priorReviewHeadSHARegex
  // in client.go). GitHub renders HTML comments as
  // nothing in the markdown view, so it's invisible to
  // human readers.
  const headMarker = `${HEAD_SHA_MARKER_PREFIX} ${ctx.prHeadSha} ${HTML_COMMENT_CLOSE}`;
  // QUB-103: per-run review id lets the runner find
  // the existing summary on retry and PATCH it instead
  // of double-posting. The UUID is generated at run()
  // start in index.mjs and threaded through ctx; tests
  // inject it via fixture ctx.
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

// postInlineComment creates a single line-specific
// review comment on the PR. Uses GitHub's "review
// comments" API which renders inline on the file diff.
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

// postInlineComments posts every line-specific comment
// in parallel. Each comment is independent, so a single
// failure does not block the rest. We use
// Promise.allSettled so failures are surfaced via the
// logger rather than thrown.
//
// QUB-103: every candidate inline is enriched with a
// deterministic `<!-- boop-inline: <path>:<line>:<body-hash> -->`
// marker before posting. On entry, the runner lists the
// PR's existing review comments, parses their markers,
// and skips any candidate whose key is already present.
// This decouples inline dedup from the fire-and-forget
// workflow-state write (QUB-92) — a pod kill that posted
// half the inlines before dying cannot cause the next
// pod to re-post them.
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

// confidenceBadge mirrors the one in lib/openrouter.mjs;
// inlined here because the comment-rendering path doesn't
// need the OpenRouter deps and tests for postReview can
// import this module without pulling in the SDK pipeline.
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
