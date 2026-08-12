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
import type { Ctx, Deps, FetchLike, Telemetry } from "../types.ts";

const POST_TIMEOUT_MS = 5000;
const POST_RETRIES = 1;

type DashboardDeps = Pick<Deps, "fetchImpl" | "log" | "errlog">;

export async function postStatus(
  stage: string,
  ctx: Ctx,
  deps: DashboardDeps,
  reason?: string,
): Promise<void> {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/status`;
  const payload: { stage: string; error?: string } = { stage };
  if (reason) payload.error = reason;
  const body = JSON.stringify(payload);
  await postWithRetry(url, body, ctx.dashboardToken, deps);
}

export async function postTelemetry(
  telemetry: Telemetry | null,
  ctx: Ctx,
  deps: DashboardDeps,
): Promise<void> {
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

export function postStage(
  stageName: string,
  ctx: Ctx,
  deps: DashboardDeps,
  opts: { ended?: boolean; meta?: Record<string, unknown> } = {},
): Promise<void> | undefined {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) return;
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/stages`;
  const body = JSON.stringify({
    stage: stageName,
    ended: !!opts.ended,
    meta: opts.meta || undefined,
  });
  return postWithRetry(url, body, ctx.dashboardToken, deps);
}

export function startHeartbeat(
  ctx: Ctx,
  deps: DashboardDeps,
): () => void {
  if (!ctx.dashboardUrl || !ctx.dashboardToken) {
    return () => {};
  }
  const url = `${ctx.dashboardUrl}/api/runs/${encodeURIComponent(ctx.jobName || jobNameFromCtx(ctx))}/heartbeat`;
  let stopped = false;
  const tick = () => {
    if (stopped) return;
    if (!ctx.dashboardToken) return;
    postWithRetry(url, "", ctx.dashboardToken, deps).catch(() => {});
  };
  const t = setInterval(tick, 30_000);
  if (typeof t.unref === "function") t.unref();
  return () => {
    if (stopped) return;
    stopped = true;
    clearInterval(t);
  };
}

export async function postLensTelemetry(
  lenses: import("../types.ts").LensTelemetry[],
  ctx: Ctx,
  deps: DashboardDeps,
): Promise<void> {
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

function jobNameFromCtx(ctx: Ctx): string {
  return ctx.jobName || `${ctx.prOwner}-${ctx.prRepo}-${ctx.prNumber}-${(ctx.prHeadSha || "").slice(0, 7)}`;
}

async function postWithRetry(
  url: string,
  body: string,
  token: string,
  deps: DashboardDeps,
): Promise<void> {
  for (let attempt = 0; attempt <= POST_RETRIES; attempt++) {
    try {
      const ac = new AbortController();
      const timer = setTimeout(() => ac.abort(), POST_TIMEOUT_MS);
      const res = await (deps.fetchImpl as FetchLike)(url, {
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
      if (res.status === 401) {
        const emit = deps.errlog || deps.log;
        emit("dashboard", "auth rejected: check BOOP_DASHBOARD_TOKEN matches the receiver's secret", {
          url,
          status: 401,
        });
        return;
      }
      if (res.status < 500 && res.status !== 202) {
        deps.log("dashboard", "post rejected", { url, status: res.status });
        return;
      }
      deps.log("dashboard", "post failed", { url, status: res.status, attempt });
    } catch (err) {
      deps.log("dashboard", "post error", { url, err: String(err instanceof Error ? err.message : err), attempt });
    }
    if (attempt < POST_RETRIES) {
      await sleep(200 * (attempt + 1));
    }
  }
}
