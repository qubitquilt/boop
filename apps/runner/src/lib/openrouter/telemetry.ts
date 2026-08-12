// Runner telemetry shape.
//
// Owns the dashboard-facing telemetry object: `buildTelemetry`
// turns an SDK call result (success or failure) into the row
// the runner POSTs to the dashboard. The field names are the
// existing contract and must not change without bumping the
// dashboard's reader.

import type { CallResult, Telemetry, SdkErrorShape } from "../../types.ts";

// QUB-105: the runner + dashboard agree on a fixed set of
// error-context fields that surface on a failed SDK call. The
// thrown Error (set in `sdk.ts` wrapSdkError) and the empty
// telemetry row (set in `buildTelemetry`) both stamp the same
// fields. `errorToTelemetryContext` is the single read-side:
// given a thrown Error, return the partial object that should
// be merged into the telemetry row. The write-side lives in
// `sdk.ts` `stampErrorContext` so both directions share the
// same field list — a future QUB-NNN that adds a fifth field
// is one edit instead of two.
//
// The Error field names and the telemetry field names are
// not identical: the Error uses `statusCode` (matching the
// SDK's typed error shape); the telemetry uses
// `errorStatusCode` (the existing dashboard contract). The
// map below is the single point of translation.
const ERROR_TO_TELEMETRY_FIELD: Record<keyof SdkErrorShape, string> = {
  statusCode: "errorStatusCode",
  errorContentType: "errorContentType",
  errorBody: "errorBody",
  durationMs: "durationMs",
  raw: "raw",
  stackDetail: "stackDetail",
};

export function stampErrorContext(
  err: SdkErrorShape & Error,
  status: number | undefined,
  contentType: string | undefined,
  body: string | undefined,
  durationMs: number | undefined,
): void {
  if (status != null) err.statusCode = status;
  if (contentType != null) err.errorContentType = contentType;
  if (body != null) err.errorBody = body;
  if (durationMs != null) err.durationMs = durationMs;
}

export function errorToTelemetryContext(error: unknown): Partial<Telemetry> {
  if (!error) return {};
  const err = error as SdkErrorShape & { message?: unknown };
  const out: Partial<Telemetry> = {
    error: String(err?.message ?? error),
  };
  for (const [errName, telName] of Object.entries(
    ERROR_TO_TELEMETRY_FIELD,
  ) as [keyof SdkErrorShape, string][]) {
    const v = (err as SdkErrorShape)[errName];
    if (v != null) {
      (out as Record<string, unknown>)[telName] = v;
    }
  }
  return out;
}

function telemetryFromUsage(
  usage: CallResult["usage"] | null | undefined,
  callResult: CallResult | null | undefined,
  error: unknown,
): Telemetry {
  const out: Telemetry = {
    model: callResult?.model || "",
    provider: "openrouter",
    inputTokens: usage?.prompt_tokens ?? 0,
    outputTokens: usage?.completion_tokens ?? 0,
    totalTokens: usage?.total_tokens ?? 0,
    reasoningTokens: usage?.reasoning_tokens ?? 0,
    cacheReadTokens: usage?.cached_tokens ?? 0,
    cacheWriteTokens: 0,
    costUsd: usage?.cost ?? 0,
    costPromptUsd: usage?.cost_prompt_usd ?? 0,
    costCompletionUsd: usage?.cost_completion_usd ?? 0,
    costUpstreamUsd: usage?.cost_upstream_usd ?? 0,
    isByok: usage?.is_byok === true,
    serverToolCallsExecuted: (usage?.server_tool_calls_executed as number) ?? 0,
    serverToolCallsRequested: (usage?.server_tool_calls_requested as number) ?? 0,
    requestId:
      callResult?.requestId ?? usage?.request_id ?? undefined,
    durationMs:
      typeof callResult?.durationMs === "number"
        ? callResult.durationMs
        : undefined,
    stepCount:
      typeof callResult?.stepCount === "number" && callResult.stepCount > 0
        ? callResult.stepCount
        : 1,
  };
  if (error) {
    Object.assign(out, errorToTelemetryContext(error));
  }
  return out;
}

/**
 * Build the runner's telemetry object from an OpenRouter
 * call result. This is the shape the runner POSTs to the
 * dashboard. The thin wrapper around `telemetryFromUsage`
 * exists so call sites stay readable: `buildTelemetry(callResult)`
 * / `buildTelemetry(null, err)` is the contract the rest of
 * the runner uses; the factory is the implementation detail.
 */
export function buildTelemetry(
  callResult: CallResult | null | undefined,
  error?: unknown,
): Telemetry {
  return telemetryFromUsage(callResult?.usage, callResult, error);
}

// emptyTelemetry returns the literal zero-valued row the
// dashboard expects on a failed call (or for any caller
// that needs to construct a row before the SDK responds).
// The shape is owned by `telemetryFromUsage`; this wrapper
// just calls it with all-null inputs. Kept as an export
// for backward compat — experts.ts and the test suite
// reach for it directly.
export function emptyTelemetry(): Telemetry {
  return telemetryFromUsage(null, null, null);
}

/**
 * Strip the opencode-internal `openrouter/` prefix from a model
 * ID so the value is acceptable to OpenRouter's own API. The
 * opencode.json ConfigMap used to store models as `openrouter/<id>`
 * because opencode uses the leading segment to pick the provider.
 * The SDK calls OpenRouter directly, so the prefix must go. After
 * QUB-98 the ConfigMap is gone and the runner sources the model
 * name from OPENROUTER_MODEL; this normalization is still useful
 * because some operators (and the receiver) keep carrying the
 * prefixed form forward.
 *
 * Returns "" for an empty input (the caller treats that as a
 * misconfiguration and throws).
 */
export function stripOpenRouterPrefix(model: string | null | undefined): string {
  if (!model) return "";
  return model.startsWith("openrouter/") ? model.slice("openrouter/".length) : model;
}
