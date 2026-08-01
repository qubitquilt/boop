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
 */
export async function postStatus(stage, ctx, deps) {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/status`;
  const body = JSON.stringify({ stage });
  await postWithRetry(url, body, ctx.dashboardToken, deps);
}

/**
 * postTelemetry sends the final token + cost rollup for a run.
 * Same best-effort contract as postStatus. Called once at the
 * end of a successful or failed review.
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
    reasoning_tokens: telemetry.reasoningTokens,
    cache_read_tokens: telemetry.cacheReadTokens,
    cache_write_tokens: telemetry.cacheWriteTokens,
    cost_usd: telemetry.costUsd,
    step_count: telemetry.stepCount,
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
