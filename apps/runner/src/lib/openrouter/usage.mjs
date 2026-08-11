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
export function extractUsage(response) {
  const usage = response?.usage;
  if (!usage || typeof usage !== "object") {
    return {
      prompt_tokens: 0,
      completion_tokens: 0,
      total_tokens: 0,
      cost: 0,
    };
  }
  // Omit optional fields when the SDK doesn't surface them.
  // Returning `{ foo: undefined }` is semantically equivalent
  // to `{}` but breaks deep-equal assertions and serialises to
  // a `null` field in JSON.
  //
  // OpenResponses (agent SDK) → OpenAI ChatUsage (chat SDK).
  // The agent SDK exposes `inputTokens` / `outputTokens`; the
  // pre-swap chat SDK exposed `promptTokens` / `completionTokens`.
  // We read both so a test fixture (or a future swap back to the
  // chat endpoint) doesn't have to mirror the change.
  const out = {
    prompt_tokens: numOrZero(usage.inputTokens ?? usage.promptTokens),
    completion_tokens: numOrZero(
      usage.outputTokens ?? usage.completionTokens,
    ),
    total_tokens: numOrZero(usage.totalTokens),
    cost: typeof usage.cost === "number" ? usage.cost : 0,
  };
  // OpenResponses nests cached / cache-write under
  // `inputTokensDetails`; the chat SDK uses
  // `promptTokensDetails`. Same with reasoning
  // (`outputTokensDetails.reasoningTokens` vs
  // `completionTokensDetails.reasoningTokens`).
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
  // cost_details splits the lumped `cost` scalar. The SDK
  // camelCases the outer key; the inner keys follow the same
  // convention. Older releases surface snake_case — try the
  // modern form first, then fall back. Any field the SDK
  // doesn't expose is omitted (the dashboard treats missing
  // as 0).
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
  // is_byok: boolean. The SDK camelCases; snake_case is the
  // pre-QUB-105 fallback. `false` is the routed-traffic default
  // (OpenRouter's own billing); `true` means a cluster operator
  // supplied their own provider key and OpenRouter only
  // forwarded the call.
  const byok = usage.isByok ?? usage.is_byok;
  if (typeof byok === "boolean") {
    out.is_byok = byok;
  }
  // server_tool_use_details: per-call tool stats. The runner
  // does not enable tools today, so the SDK reports zeros; we
  // forward whatever the SDK exposes so a future tool-using
  // skill does not need a runner-side schema change.
  const serverTools =
    usage.serverToolUseDetails ?? usage.server_tool_use_details;
  if (serverTools && typeof serverTools === "object") {
    const executed =
      serverTools.toolCallsExecuted ?? serverTools.tool_calls_executed;
    const requested =
      serverTools.toolCallsRequested ?? serverTools.tool_calls_requested;
    if (typeof executed === "number") {
      out.server_tool_calls_executed = executed;
    }
    if (typeof requested === "number") {
      out.server_tool_calls_requested = requested;
    }
  }
  // Response-level id (OpenRouter's per-request identifier).
  // The SDK stamps `id` on the ChatResult, not on usage;
  // pull it here so the caller's callResult shape carries it.
  if (typeof response?.id === "string") {
    out.request_id = response.id;
  }
  return out;
}

function numOrZero(v) {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}
