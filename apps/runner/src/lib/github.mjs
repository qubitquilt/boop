// GitHub API surface (shim, RF-020 split).
//
// The 928-LOC github.mjs file owned ten concerns: token
// mint, graphql helper, status PATCH, status body
// template, ensure-status-comment, final reaction,
// workflow-state markers, inline-key markers, octokit
// factory, postReview, postInlineComments, cleanupPrior.
// After the split, three focused modules own them:
//
//   - ./github/auth.mjs        mintInstallationToken,
//                               makeOctokit, graphql,
//                               cleanupPriorReview +
//                               helpers (auth/lifecycle)
//   - ./github/comments.mjs    postStatus, postReview,
//                               postInlineComments,
//                               ensureStatusComment,
//                               postFinalReaction,
//                               renderInitialStatusBody
//                               (visible surface)
//   - ./github/markers.mjs     WORKFLOW_STATE_MARKER,
//                               REVIEW_ID_MARKER_PREFIX,
//                               INLINE_KEY_MARKER_PREFIX,
//                               HEAD_SHA_MARKER_PREFIX,
//                               readWorkflowState,
//                               writeWorkflowState,
//                               inlineKeyForComment,
//                               appendInlineKeyMarker,
//                               parseInlineKey,
//                               findExistingSummaryCommentID,
//                               listExistingInlineKeys
//                               (dedup concerns)
//
// This file re-exports the public surface so existing
// callers (workflow.mjs, index.mjs, workflow.test.mjs)
// keep their import contract. New code should import
// from the focused module directly.
//
// STATUS and SHORT (re-exported from config.mjs at the
// top) stay here because the test suite reaches for
// them as the user-visible surface for status labels.

export { STATUS, SHORT } from "./config.mjs";

export { mintInstallationToken, makeOctokit, cleanupPriorReview } from "./github/auth.mjs";

export {
  postStatus,
  renderInitialStatusBody,
  ensureStatusComment,
  postFinalReaction,
  postReview,
  postInlineComments,
} from "./github/comments.mjs";

export {
  WORKFLOW_STATE_MARKER,
  REVIEW_ID_MARKER_PREFIX,
  INLINE_KEY_MARKER_PREFIX,
  HEAD_SHA_MARKER_PREFIX,
  readWorkflowState,
  writeWorkflowState,
  inlineKeyForComment,
  appendInlineKeyMarker,
  parseInlineKey,
  listExistingInlineKeys,
  findExistingSummaryCommentID,
} from "./github/markers.mjs";
