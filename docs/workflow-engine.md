# Workflow engine for PR review

This document picks the workflow engine that drives the staged PR review
(QUB-87). It captures the tradeoffs so a future change can revisit the
choice with the same evidence.

## The shape of the work

The runner today is one process. It runs five macro steps in order:

1. Mint a GitHub App installation token.
2. Clone the PR.
3. Run the multi-lens LLM review.
4. Post the summary comment.
5. Post the inline comments.

Step 3 ("run the review") is itself a multi-agent workflow. The LLM
classifies the PR, dispatches one or more tool-using experts in
parallel (each with bash, file-read, and test-runner access scoped to
the PR), collects the findings, runs a meta-review to catch things
that "stick out as potentially wrong" with a bounded re-pass, and
finally produces a single cohesive narrative. The macro step 3 is
the `sniff` stage; the five inner phases are the `review`
sub-workflow.

A failure at any step kills the run. There is no partial resume. The
status thread shows progress, but the controller behind it is opaque.

The target shape is a two-level state machine. At the macro level
the runner walks five ordered stages. At the sub level, the `sniff`
macro-stage walks a sub-workflow of five ordered sub-stages, with a
bounded re-pass loop. Each stage and sub-stage has a quality gate.
Each has bounded retry. Each is individually resumable.

## Macro-stages (top level)

| id | status label | what it does |
|---|---|---|
| `handshake` | `auth` | Mint a GitHub App installation token. |
| `fetch`     | `clone` | Clone the PR at the head SHA. |
| `sniff`     | `review` | Run the multi-expert review sub-workflow. |
| `summary`   | _(silent)_ | Post the summary comment. |
| `inlines`   | _(silent)_ | Post the inline comments. |
| `cleanup`   | _(silent)_ | Resolve / minimize prior Boop artifacts on re-reviews. |

`_run_opencode_` in the macro `sniff` stage is no longer a single
LLM call; it is the sub-workflow executor. The "review" status
line is posted once at the start of the macro `sniff` stage and
covers the whole sub-workflow (so the user-visible surface stays
the same; see QUB-93).

## Sub-stages inside `sniff` (review sub-workflow)

| id | what it does |
|---|---|
| `classify`    | Identify the type of PR (bug fix, feature, refactor, docs, test-only, infra, etc.). The classification drives the expert selection in the next sub-stage. |
| `dispatch`    | Choose the experts (1..N) and craft the context for each. Run them in parallel as independent `opencode run` invocations with a tool surface scoped to the PR (bash, file reads, test runner). Each expert is autonomous — it can run tests, inspect files, etc. |
| `gather`      | Collect the findings from every expert into a single `state.findings` array. |
| `meta-review` | A meta-reviewer LLM call that scans the findings for things that "stick out as potentially wrong" (false positives, contradictions, missing context). If anything sticks out, dispatch a re-pass of the specific experts that produced those findings. The re-pass is **bounded to one re-pass per run**; meta-review cannot re-loop. |
| `narrate`     | Once meta-review is satisfied, a narrative-writer LLM call produces a single cohesive summary + the inline-comment set from the (possibly re-reviewed) findings. |

The bounded re-pass is the cost-control knob. Worst-case expert
cost is `2 * (initial + re-pass for some subset)`. Wall time stays
inside the 30-min `activeDeadlineSeconds` because expert calls run
in parallel within each pass.

## Candidates

| Engine | Guarantees | Cost | New infra | Misfit |
|---|---|---|---|---|
| Temporal | Strong (durable timers, signals, child workflows) | New control plane + workers | Temporal cluster + DB | Too much; the macro workflow has 6 stages + a sub-workflow with 5 sub-stages. Temporal pays off when the workflow is long-lived or the team needs queryable state. |
| Custom K8s controller | Full control | New controller + CRDs | Controller Deployment + RBAC | More Go code than the runner itself; we would be re-implementing a workflow engine for one workflow. |
| Argo Workflows | Already in cluster (or trivially added) | Moderate | Argo Workflows CRDs + controller | Limited retry policy per step; resume semantics are WorkflowTemplate-version-sensitive. A WorkflowTemplate change while a run is in flight can orphan the run. The DAG / fan-out it provides is appealing for the parallel experts, but we'd still be wiring state into a side store. |
| Stateful Job chain | K8s-native | Minimal | None | The orchestrator lives in the runner pod; state is persisted outside the pod. Resume depends on whatever external store we pick. |

## Choice

**Stateful Job chain. State lives in the GitHub status comment.**

The runner becomes an internal state machine. Macro-stages are a
flat list in `apps/runner/src/lib/workflow.mjs`. The `sniff`
macro-stage's `run` is a sub-workflow executor that walks a flat
list of sub-stages. A small `withRetry` helper applies a
bounded-attempt / exponential-backoff policy to any stage or
sub-stage. The runner persists the list of passed stages (and
passed sub-stages) to the existing GitHub status comment (in a
hidden HTML comment, e.g.
`<!-- boop-state: {"macro":["handshake","fetch"],"sub":{"sniff":["classify"]}} -->`).

Resume falls out of the existing dedup logic in the receiver:

> "Job already failed for head SHA → delete the failed Job, submit a
> new one." (see [webhook-contract.md](./webhook-contract.md#job-naming-and-dedup))

A re-submitted Job for the same head SHA inherits the same
`BOOP_STATUS_COMMENT_ID`. The new runner reads the previous state
from the comment, skips the macro-stages (and sub-stages) that
already passed, and resumes from the first not-passed (sub-)stage.

## Why this fits

- **No new infra.** The runner is already a single K8s Job per PR; we
  keep that shape. The state is already in a place the runner writes
  (the status comment). The re-trigger path already exists in the
  receiver's dedup.
- **Reuses durable GitHub storage.** The status comment is the same
  surface users already see. A failed run's state is not lost when the
  pod dies — it lives in the comment thread.
- **Idempotency surface is small.** The runner's externally-visible
  writes are: status-comment PATCHes, summary-comment POST, inline
  comments POST, resolve/minimize GraphQL calls. The runner already
  swallows status-comment PATCH failures (best-effort). The other
  writes need explicit idempotency on resume (see below).
- **No new RBAC.** The runner's service account is unchanged.
- **No new container images.** The runner image is unchanged; the
  receiver image is unchanged; the ConfigMap is unchanged.
- **Parallel experts are native.** Each expert is a separate
  `opencode run` invocation within the same runner pod. Node's
  `Promise.all` fans them out; the pod's CPU/memory budget covers
  them. The 30-min `activeDeadlineSeconds` is the wall-clock ceiling.

## Tradeoffs

- **State is best-effort.** A status-comment PATCH that fails leaves
  the runner's in-memory state ahead of the comment. The next stage
  PATCH will overwrite, but a hard pod kill between two PATCHes means
  a re-trigger sees a slightly stale state. Mitigation: the runner
  re-derives the "passed" list from the status-comment timeline as a
  fallback. Any status line in the comment for a stage whose output
  side effects (summary post, inlines post) are still in GitHub counts
  as "passed".
- **Idempotency for summary and inline comments.** Re-running the
  summary stage would post a duplicate summary comment. Re-running the
  inlines stage would post duplicate inline comments. The per-stage
  gate must check whether the side effect already landed in GitHub
  before re-running:
  - Summary: look for an existing bot comment whose body contains the
    `<!-- boop-head-sha: <sha> -->` marker for this head SHA. If
    present, treat the stage as passed and reuse the comment ID.
  - Inlines: list the PR's existing review comments, intersect with
    the candidate set by `(path, line, body)`. Skip the duplicates.
  - This work is part of the resume PR (QUB-92), not the engine
    choice. The engine just needs the gate to be plumbable.
- **Status-comment size.** The state marker adds ~200 bytes per run
  (a JSON blob with macro + sub arrays). The comment trim path
  already handles 60 KB; this is well under the budget.
- **Single comment per head SHA.** The dedup key is the head SHA, so
  a single review = a single comment = a single state container. No
  fan-out concerns.
- **Bounded re-pass.** The meta-review sub-stage can dispatch at most
  one re-pass per run. The user explicitly chose this bound. A
  future ticket can revisit if real PRs need more iterations; the
  hook is plumbed.
- **Expert cost is variable.** The orchestrator can pick 1..N
  experts. The cost model is "1 classifier + 1 orchestrator + N
  experts + 1 meta-reviewer + 1 narrative-writer + (optionally) N
  re-pass experts." The Dashboard data layer will show the actual
  cost per run; if it balloons, the operator can constrain the
  orchestrator (cap N, or constrain the expert pool).

## What we are not picking, and why

- **Temporal.** The control plane is the dominant cost. We have one
  macro workflow with a sub-workflow; Temporal is overkill. We can
  revisit if the workflow grows (long-running reviews, multi-PR
  aggregations).
- **Custom K8s controller.** A controller is justified when many
  workflows share the same engine. Boop has one macro workflow; the
  runner is the engine. A controller would be a layer of indirection
  on top of the runner, not a replacement.
- **Argo Workflows.** Argo is the natural fit if the team already
  runs it for other workflows and is comfortable with its retry and
  resume semantics. Today we do not depend on Argo for anything else;
  adding it for one workflow is a net increase in moving parts. The
  DAG / fan-out it provides is genuinely useful for the parallel
  experts, but Node's `Promise.all` covers that with no new
  infrastructure.

## Migration shape

The migration lands in nine stacked PRs. Each PR is self-contained
and the existing test suite passes end-to-end at every step.

### Macro-stages (the original QUB-87 plan)

1. **QUB-88 — spike (this PR).** Engine choice + sub-workflow shape.
2. **QUB-89 — define macro-stages.** Refactor `run()` so each macro
   stage is a named async function. No behavior change. New
   `apps/runner/src/lib/workflow.mjs` exports the macro list and the
   executor. The `sniff` macro-stage is a thin wrapper around the
   sub-workflow (initially a single-step sub-workflow that calls the
   current `runOpenCodeSkill` — same behavior as before).
3. **QUB-90 — add gates.** Each macro stage gets a gate that checks
   its output. Gate failures are logged and surface in the status
   thread. No behavior change on the happy path; the new code path
   is exercised by new tests.
4. **QUB-91 — add retry.** A `withRetry(stage, fn, opts)` helper
   applies a bounded-attempt / exponential-backoff policy. Behavior
   change: a transient stage failure retries instead of failing the
   run. Tunable via `BOOP_STAGE_MAX_ATTEMPTS` and
   `BOOP_STAGE_BACKOFF_BASE_MS`.
5. **QUB-92 — add resume.** The runner reads the passed-stages list
   from the status comment on startup and skips already-passed
   stages. The summary and inlines gates check GitHub for existing
   side effects before re-running.
6. **QUB-93 — keep status thread parity.** A new test pins the
   status-thread surface (emoji, short labels, ordering) so the
   staged runner produces the same user-visible thread as today's
   one-shot runner. The sub-stages inside `sniff` are silent (no
   new status lines) — the existing "review" status line covers
   the whole sub-workflow.

### Sub-stages (the multi-expert review)

7. **QUB-94 — classify.** Add the `classify` sub-stage. The
   classifier is a small LLM call (or a deterministic check) that
   identifies the PR type. Its output drives the expert selection
   in `dispatch`.
8. **QUB-95 — dispatch + parallel experts.** Add the `dispatch`,
   `gather`, and `narrate` sub-stages. The orchestrator picks
   experts; the runner fans them out as parallel `opencode run`
   invocations; `gather` collects the findings; `narrate` produces
   the summary + inlines. This is the most expensive PR (token
   cost and wall time both grow).
9. **QUB-96 — meta-review with bounded re-pass.** Add the
   `meta-review` sub-stage. The meta-reviewer scans the gathered
   findings; if anything sticks out, it dispatches a re-pass of
   the specific experts that produced those findings. Bounded to
   one re-pass per run.

## Open question for the team

The summary comment's head-SHA marker already exists (see
`apps/runner/src/lib/github.mjs` `postReview`). The summary gate can
reuse it to detect a duplicate summary on resume. The inlines gate has
no equivalent marker today; the simplest dedupe is
`(path, line, body)`-exact. Confirm the dedupe rule before the resume
PR (QUB-92) lands.
