// Slot primitives for the runner's dependency-injection
// container (RF-010).
//
// The runner's `makeDeps` (lib/index.mjs) has two slot
// fields: the Octokit instance (populated by the handshake
// stage after the GitHub App installation token is minted)
// and the status comment id (lazily populated by the first
// postStatus call). The slot pattern lets stage functions
// in workflow.mjs read the live values without reaching
// back into index.mjs to mutate module state.
//
// Before this refactor, the slots were two ad-hoc
// `{ value: ... }` objects in makeDeps, with the
// getters/setters inlined as closures. After the refactor,
// the slots live here as small classes with the read/write
// surface named (set/get/applyTo). makeDeps shrinks by ~25
// LOC and the slot pattern is reusable for any future
// runner state that follows the same shape (e.g. a
// tooling-enabled flag, a model-name override, etc.).
//
// Tests can construct a slot, set a value, and assert on
// the getter — the pattern is unit-testable in isolation.
// The runner's existing tests inject deps via a stub
// (`recordingDeps` in workflow.test.mjs) and never
// construct a slot directly; the slot class is the
// production wiring.

/**
 * OctokitSlot holds the GitHub App installation's Octokit
 * instance. The handshake stage populates it after the
 * token mint; downstream stage functions (postStatus,
 * readWorkflowState, writeWorkflowState) read it.
 *
 * isReady() is the explicit "do we have a token yet?"
 * check so postStatus can skip with a log line instead of
 * calling the GitHub API with a null octokit.
 */
export class OctokitSlot {
  constructor() {
    this.value = null;
  }
  get() {
    return this.value;
  }
  set(octokit) {
    this.value = octokit;
  }
  isReady() {
    return this.value != null;
  }
}

/**
 * StatusCommentSlot holds the live status comment id.
 * The receiver no longer pre-creates the comment
 * (QUB-99); the first postStatus call lazy-creates the
 * comment and caches the id here, so every subsequent
 * PATCH targets the same comment instead of posting a new
 * one.
 *
 * applyTo(ctx) is the read-side helper: returns a copy of
 * ctx with `statusCommentId` set to the live value when
 * the slot is populated, or ctx unchanged when the slot
 * is still null. Callers that need the live id (e.g.
 * readWorkflowState, writeWorkflowState) call applyTo
 * before they read state.
 */
export class StatusCommentSlot {
  constructor(initialValue) {
    this.value = initialValue || null;
  }
  get() {
    return this.value;
  }
  set(id) {
    this.value = id;
  }
  applyTo(ctx) {
    return this.value ? { ...ctx, statusCommentId: this.value } : ctx;
  }
}
