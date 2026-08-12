// OpenRouter usage mapper.
//
// Pure OpenResponses ↔ ChatUsage shape translation. The agent
// SDK returns the OpenResponses `Usage` shape (camelCase:
// `inputTokens`, `costDetails`, `isByok`, ...); the pre-swap chat
// shape used `promptTokens` / `completionTokens` / `cost_details`.
// Both are supported so test fixtures keep passing.
//
// Output is snake_case — the contract `extractUsage` →
// `buildTelemetry` → dashboard reads. The dashboard is the
// load-bearing consumer: changing a field name here is a wire
// change for the receiver.

import type { ChatResult, UsageLike } from "../../types.ts";

/**
 * Map the SDK `usage` object onto the runner's telemetry shape.
 *
 * The agent SDK returns the OpenResponses `Usage` shape
 * (camelCase: `inputTokens`, `costDetails`, `isByok`, ...); the
 * pre-swap chat shape used `promptTokens` / `completionTokens` /
 * `cost_details`. Both are supported so test fixtures keep passing.
 * Output is snake_case — the contract `extractUsage` →
 * `buildTelemetry` → dashboard reads.
 */
export function extractUsage(response: ChatResult | null | undefined): UsageLike {
  const usage = response?.usage;
  if (!usage || typeof usage !== "object") {
    return {
      prompt_tokens: 0,
      completion_tokens: 0,
      total_tokens: 0,
      cost: 0,
    };
  }
  const out: UsageLike = {
    prompt_tokens: numOrZero(usage.inputTokens ?? usage.promptTokens),
    completion_tokens: numOrZero(
      usage.outputTokens ?? usage.completionTokens,
    ),
    total_tokens: numOrZero(usage.totalTokens),
    cost: typeof usage.cost === "number" ? usage.cost : 0,
  };
  const cached =
    usage.inputTokensDetails?.cachedTokens ??
    usage.promptTokensDetails?.cachedTokens;
  if (cached != null) {
    out.cached_tokens = numOrZero(cached);
  }
  const cacheWrite =
    usage.inputTokensDetails?.cacheWriteTokens ??
    usage.promptTokensDetails?.cache_write_tokens;
  if (cacheWrite != null) {
    out.cache_write_tokens = numOrZero(cacheWrite);
  }
  const reasoning =
    usage.outputTokensDetails?.reasoningTokens ??
    usage.completionTokensDetails?.reasoningTokens;
  if (reasoning != null) {
    out.reasoning_tokens = numOrZero(reasoning);
  }
  const costDetails = usage.costDetails ?? usage.cost_details;
  if (costDetails && typeof costDetails === "object") {
    const promptCost =
      costDetails.upstreamInferencePromptCost ??
      costDetails.upstreamInferenceInputCost ??
      costDetails.upstream_inference_prompt_cost;
    const completionCost =
      costDetails.upstreamInferenceCompletionsCost ??
      costDetails.upstreamInferenceOutputCost ??
      costDetails.upstream_inference_completions_cost;
    const upstreamCost =
      costDetails.upstreamInferenceCost ??
      costDetails.upstream_inference_cost;
    if (typeof promptCost === "number") {
      out.cost_prompt_usd = promptCost;
    }
    if (typeof completionCost === "number") {
      out.cost_completion_usd = completionCost;
    }
    if (typeof upstreamCost === "number") {
      out.cost_upstream_usd = upstreamCost;
    }
  }
  const byok = usage.isByok ?? usage.is_byok;
  if (typeof byok === "boolean") {
    out.is_byok = byok;
  }
  const serverTools =
    usage.serverToolUseDetails ?? usage.server_tool_use_details;
  if (serverTools && typeof serverTools === "object") {
    const s = serverTools as {
      toolCallsExecuted?: unknown;
      tool_calls_executed?: unknown;
      toolCallsRequested?: unknown;
      tool_calls_requested?: unknown;
    };
    const executed = s.toolCallsExecuted ?? s.tool_calls_executed;
    const requested = s.toolCallsRequested ?? s.tool_calls_requested;
    if (typeof executed === "number") {
      out.server_tool_calls_executed = executed;
    }
    if (typeof requested === "number") {
      out.server_tool_calls_requested = requested;
    }
  }
  if (typeof response?.id === "string") {
    out.request_id = response.id;
  }
  return out;
}

function numOrZero(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}
