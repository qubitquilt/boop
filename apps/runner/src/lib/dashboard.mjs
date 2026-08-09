// dashboard: best-effort POST helpers that the runner uses to
// report lifecycle + telemetry to the receiver's data-layer
// endpoints. The receiver is in-cluster and reachable via
// BOOP_DASHBOARD_URL (the receiver Service's ClusterIP).
//
// Every call is best-effort: failures are logged but never
// raised. The runner's job is to review the PR; if the dashboard
// can't be reached, the review still completes. The next stage
// transition retries the POST, so a transient blip is
// self-healing.
//
// Auth: the receiver's data-layer POSTs require
// X-BOOP-Runner-Token. The secret is set once via the receiver
// deployment's Secret and propagated to the runner through
// BOOP_DASHBOARD_TOKEN in the Job template. We use constant-time
// compare in the receiver; on the runner side we just send the
// header.

import { setTimeout as sleep } from "node:timers/promises";

const POST_TIMEOUT_MS = 5000;
const POST_RETRIES = 1;

/**
 * postStatus tells the receiver that the run advanced to a new
 * stage (auth/clone/review/done/failed). Best-effort: a 4xx
 * response is logged at warn; a 5xx or network error is retried
 * once after a short backoff, then logged and dropped.
 *
 * `reason` is an optional free-form string attached to the
 * status payload as `error`. QUB-102: the runner's abort path
 * stashes the abort reason on `state.failureReason` and forwards
 * it here so the operator's dashboard row distinguishes a
 * duplicate-pod abort from a sniff-parse failure (both reach
 * the receiver as stage="failed"; the reason disambiguates).
 */
export async function postStatus(stage, ctx, deps, reason) {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/status`;
  const payload = { stage };
  if (reason) payload.error = reason;
  const body = JSON.stringify(payload);
  await postWithRetry(url, body, ctx.dashboardToken, deps);
}

/**
 * postTelemetry sends the final token + cost rollup for a run.
 * Same best-effort contract as postStatus. Called once at the
 * end of a successful or failed review.
 *
 * QUB-105: the payload carries every QUB-105 field
 * (`total_tokens`, `cost_prompt_usd`, `cost_completion_usd`,
 * `is_byok`, `server_tool_calls_*`, `request_id`, `duration_ms`,
 * `error_status_code`, `error_content_type`, `error_body`).
 * `JSON.stringify` drops `undefined` values, so a successful
 * call with no error fields serialises a clean object — the
 * receiver's `telemetryRequest` accepts the new fields as
 * optional and the SQL columns have defaults, so a partial
 * payload (older runner, missing fields) lands cleanly.
 */
export async function postTelemetry(telemetry, ctx, deps) {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  if (!telemetry) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/telemetry`;
  const body = JSON.stringify({
    model: telemetry.model,
    provider: telemetry.provider,
    input_tokens: telemetry.inputTokens,
    output_tokens: telemetry.outputTokens,
    total_tokens: telemetry.totalTokens,
    reasoning_tokens: telemetry.reasoningTokens,
    cache_read_tokens: telemetry.cacheReadTokens,
    cache_write_tokens: telemetry.cacheWriteTokens,
    cost_usd: telemetry.costUsd,
    cost_prompt_usd: telemetry.costPromptUsd,
    cost_completion_usd: telemetry.costCompletionUsd,
    cost_upstream_usd: telemetry.costUpstreamUsd,
    is_byok: telemetry.isByok === true,
    server_tool_calls_executed: telemetry.serverToolCallsExecuted,
    server_tool_calls_requested: telemetry.serverToolCallsRequested,
    request_id: telemetry.requestId,
    duration_ms: telemetry.durationMs,
    step_count: telemetry.stepCount,
    error: telemetry.error,
    error_status_code: telemetry.errorStatusCode,
    error_content_type: telemetry.errorContentType,
    error_body: telemetry.errorBody,
  });
  await postWithRetry(url, body, ctx.dashboardToken, deps);
}

// QUB-109: per-stage POST for the waterfall. The receiver
// stamps started_at and ended_at with its own clock — the
// runner passes only the stage name and a "this is the
// exit" signal. The receiver overwrites any client-supplied
// started_at so the waterfall bars line up across stages
// that span pods (hmac_verify runs in the receiver,
// pod_schedule runs in the K8s API, comment_post runs in
// the runner).
//
// `meta` is the per-stage escape hatch: comment_post stages
// pass {"path":"...","line":N}, lens stages pass
// {"model":"...","tokens":N}. Stored verbatim in
// run_stages.meta; the dashboard reads it for the inline-
// comment map and the lens self-tag.
//
// QUB-109 calls are fire-and-forget by design — the runner
// posts the START of a stage, then immediately fires the
// async work. The END POST lands from a finally /
// setImmediate / Promise.then at the natural exit point.
// The receiver's UNIQUE(run_id, stage) constraint makes
// re-delivery safe: a re-post overwrites start without
// losing end.
export function postStage(stageName, ctx, deps, opts = {}) {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/stages`;
  const body = JSON.stringify({
    stage: stageName,
    ended: !!opts.ended,
    meta: opts.meta || undefined,
  });
  return postWithRetry(url, body, ctx.dashboardToken, deps);
}

/**
 * startHeartbeat kicks off a 30s interval that POSTs to
 * /api/runs/:id/heartbeat. The receiver stamps last_heartbeat_at
 * with its own clock; the runner's clock is irrelevant. The
 * stuck-runs panel reads the gap (no heartbeat in 2 minutes
 * while status=running = "stuck").
 *
 * Returns a stop function. Callers MUST invoke it in a
 * finally block — leaking a timer across a successful run
 * keeps the process alive past its work. The stop is
 * idempotent.
 *
 * QUB-109: a hung LLM call keeps heartbeating (the
 * setInterval fires while the LLM awaits) but never
 * advances the stage. A crashed pod stops heartbeating.
 * These are different operator responses (re-queue vs
 * investigate model latency) and the spec's
 * "distinguishes a hung LLM call from a crashed pod"
 * rule is exactly what the heartbeat/stage-emission
 * split captures.
 */
export function startHeartbeat(ctx, deps) {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) {
    return () => {};
  }
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/heartbeat`;
  let stopped = false;
  const tick = () => {
    if (stopped) return;
    // postWithRetry is fire-and-forget; failures are
    // logged inside the helper. The timer is the source
    // of the stuck-runs signal, not any single tick's
    // success.
    postWithRetry(url, "", ctx.dashboardToken, deps).catch(() => {});
  };
  // First tick after 30s so the receiver's UpsertRun has
  // a chance to land — an immediate tick would land a 202
  // (run not persisted) and retry forever. The receiver's
  // stuck-runs panel uses a 2-minute threshold, so a 30s
  // first tick still gets two more attempts before
  // "stuck" lights up.
  const t = setInterval(tick, 30_000);
  // Don't keep the event loop alive just for the heartbeat.
  if (typeof t.unref === "function") t.unref();
  return () => {
    if (stopped) return;
    stopped = true;
    clearInterval(t);
  };
}

/**
 * postLensTelemetry posts a batch of per-lens rollups at
 * end-of-run. The runner parses `lens: <name>` markers
 * from the orchestrator's output and accumulates one
 * rollup per lens; the receiver REPLACES the per-lens
 * rows for the run so a re-run / re-delivery lands on
 * the same shape the dashboard expects.
 *
 * Decouples attribution from prompt layout — the
 * meta-review refactor in QUB-96 won't break this as
 * long as the orchestrator still emits the markers.
 */
export async function postLensTelemetry(lenses, ctx, deps) {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  if (!Array.isArray(lenses) || lenses.length === 0) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/lens_telemetry`;
  const body = JSON.stringify({
    lenses: lenses.map((l) => ({
      lens: l.lens,
      model: l.model,
      provider: l.provider,
      input_tokens: l.inputTokens || 0,
      output_tokens: l.outputTokens || 0,
      reasoning_tokens: l.reasoningTokens || 0,
      cache_read_tokens: l.cacheReadTokens || 0,
      cache_write_tokens: l.cacheWriteTokens || 0,
      cost_usd: l.costUsd || 0,
      step_count: l.stepCount || 0,
    })),
  });
  await postWithRetry(url, body, ctx.dashboardToken, deps);
}

function jobNameFromCtx(ctx) {
  // ctx.jobName is set by the runner from the pod's downward-API
  // JOB_NAME (or from the PR's owner/repo/number/sha as a
  // fallback). Either way, the receiver uses the same id it
  // generated when the Job was created, so any path that lands
  // in the same shape works.
  return ctx.jobName || `${ctx.prOwner}-${ctx.prRepo}-${ctx.prNumber}-${(ctx.prHeadSha || "").slice(0, 7)}`;
}

async function postWithRetry(url, body, token, deps) {
  for (let attempt = 0; attempt <= POST_RETRIES; attempt++) {
    try {
      const ac = new AbortController();
      const timer = setTimeout(() => ac.abort(), POST_TIMEOUT_MS);
      const res = await deps.fetchImpl(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-BOOP-Runner-Token": token,
        },
        body,
        signal: ac.signal,
      });
      clearTimeout(timer);
      if (res.status >= 200 && res.status < 300) return;
      // 202 (run not yet persisted) and 4xx (auth/validation)
      // are not retryable. 5xx is.
      if (res.status < 500 && res.status !== 202) {
        deps.log("dashboard", "post rejected", { url, status: res.status });
        return;
      }
      deps.log("dashboard", "post failed", { url, status: res.status, attempt });
    } catch (err) {
      deps.log("dashboard", "post error", { url, err: String(err?.message ?? err), attempt });
    }
    if (attempt < POST_RETRIES) {
      await sleep(200 * (attempt + 1));
    }
  }
}
