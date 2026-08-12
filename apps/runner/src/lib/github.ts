// GitHub API surface (shim, RF-020 split).
//
// The 928-LOC github.ts file owned ten concerns: token
// mint, graphql helper, status PATCH, status body
// template, ensure-status-comment, final reaction,
// workflow-state markers, inline-key markers, octokit
// factory, postReview, postInlineComments, cleanupPrior.
// After the split, three focused modules own them:
//
//   - ./github/auth.ts        mintInstallationToken,
//                               makeOctokit, graphql,
//                               cleanupPriorReview +
//                               helpers (auth/lifecycle)
//   - ./github/comments.ts    postStatus, postReview,
//                               postInlineComments,
//                               ensureStatusComment,
//                               postFinalReaction,
//                               renderInitialStatusBody
//                               (visible surface)
//   - ./github/markers.ts     WORKFLOW_STATE_MARKER,
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
// callers (workflow.ts, index.ts, workflow.test.ts)
// keep their import contract. New code should import
// from the focused module directly.
//
// STATUS and SHORT (re-exported from config.ts at the
// top) stay here because the test suite reaches for
// them as the user-visible surface for status labels.

export { STATUS, SHORT } from "./config.ts";

export {
  mintInstallationToken,
  makeOctokit,
  cleanupPriorReview,
} from "./github/auth.ts";

export {
  postStatus,
  renderInitialStatusBody,
  ensureStatusComment,
  postFinalReaction,
  postReview,
  postInlineComments,
} from "./github/comments.ts";

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
} from "./github/markers.ts";
