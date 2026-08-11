// Runner telemetry shape.
//
// Owns the dashboard-facing telemetry object: `buildTelemetry`
// turns an SDK call result (success or failure) into the row
// the runner POSTs to the dashboard. The field names are the
// existing contract and must not change without bumping the
// dashboard's reader.

// QUB-105: the runner + dashboard agree on a fixed set of
// error-context fields that surface on a failed SDK call. The
// thrown Error (set in `sdk.mjs` wrapSdkError) and the empty
// telemetry row (set in `buildTelemetry`) both stamp the same
// fields. `errorToTelemetryContext` is the single read-side:
// given a thrown Error, return the partial object that should
// be merged into the telemetry row. The write-side lives in
// `sdk.mjs` `stampErrorContext` so both directions share the
// same field list — a future QUB-NNN that adds a fifth field
// is one edit instead of two.
//
// The Error field names and the telemetry field names are
// not identical: the Error uses `statusCode` (matching the
// SDK's typed error shape); the telemetry uses
// `errorStatusCode` (the existing dashboard contract). The
// map below is the single point of translation.
const ERROR_TO_TELEMETRY_FIELD = {
  statusCode: "errorStatusCode",
  errorContentType: "errorContentType",
  errorBody: "errorBody",
  durationMs: "durationMs",
};

function stampErrorContext(err, status, contentType, body, durationMs) {
  if (status != null) err.statusCode = status;
  if (contentType != null) err.errorContentType = contentType;
  if (body != null) err.errorBody = body;
  if (durationMs != null) err.durationMs = durationMs;
}

function errorToTelemetryContext(error) {
  if (!error) return {};
  const out = {
    error: String(error?.message ?? error),
  };
  for (const [errName, telName] of Object.entries(
    ERROR_TO_TELEMETRY_FIELD,
  )) {
    const v = error[errName];
    if (v != null) {
      out[telName] = v;
    }
  }
  return out;
}

export { stampErrorContext, errorToTelemetryContext };

/**
 * The single source of truth for the runner's telemetry
 * shape (RF-014). Both `buildTelemetry` (success path) and
 * `emptyTelemetry` (no-result path) used to be 60+ LOC of
 * literal maps with overlapping field names; this factory
 * owns the literal map once and the two callers are thin
 * wrappers that pick the inputs.
 *
 * Inputs:
 *   - usage: the SDK's `usage` object (camelCase or
 *     snake_case per the agent SDK / chat SDK split handled
 *     in `usage.mjs`). `null` → all numeric fields default
 *     to 0, isByok defaults to false, requestId / durationMs
 *     default to undefined.
 *   - callResult: the SDK's full call result, used for
 *     callResult.model, callResult.requestId,
 *     callResult.durationMs, callResult.stepCount. `null`
 *     → these default to "" / undefined / 1.
 *   - error: a thrown Error from the SDK (the non-ok path).
 *     When present, QUB-105 fields are merged in via
 *     `errorToTelemetryContext`. `null` → no error context.
 *
 * Field names are the existing dashboard contract; the
 * factory is the only place they are listed. A future
 * QUB-NNN that adds a field is one edit here, not two
 * (the empty + success maps used to drift).
 */
function telemetryFromUsage(usage, callResult, error) {
  const out = {
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
    serverToolCallsExecuted: usage?.server_tool_calls_executed ?? 0,
    serverToolCallsRequested: usage?.server_tool_calls_requested ?? 0,
    requestId: callResult?.requestId ?? usage?.request_id ?? undefined,
    durationMs: typeof callResult?.durationMs === "number" ? callResult.durationMs : undefined,
    // QUB-<next: surface the actual agent-loop step count
    // (callOpenRouter sets stepCount = toolCalls.length + 1
    // when tools were passed). Falls back to 1 for legacy
    // callers that hand in a callResult without a stepCount
    // field — the dashboard contract stays "stepCount is a
    // non-null integer."
    stepCount:
      typeof callResult?.stepCount === "number" && callResult.stepCount > 0
        ? callResult.stepCount
        : 1,
  };
  if (error) {
    // QUB-105: when callOpenRouter attaches status /
    // content-type / body / duration to the thrown Error
    // (the non-ok path), stamp them on the telemetry row
    // so the operator can diagnose without a pod-log round
    // trip. RF-005: the field set is defined in
    // `errorToTelemetryContext` so the write-side
    // (sdk.mjs wrapSdkError) and this read-side stay in
    // sync.
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
 *
 * Returns the empty telemetry object when the call failed
 * before the response landed (timeout, 4xx/5xx, etc.) so
 * the dashboard still gets a row. The failure-mode rows
 * carry the QUB-105 error context (status / content-type /
 * body) so a 4xx is diagnosable from the dashboard.
 *
 * Distinguishes a failed SDK call from a successful call
 * that happened to produce an empty summary: the dashboard
 * can filter on `error` to separate "model said nothing
 * useful" from "the API rejected the request". `error` is
 * a short string and intentionally NOT counted as telemetry
 * — the cost / token fields stay zero so the failure doesn't
 * double-count if the dashboard later sums across runs.
 */
export function buildTelemetry(callResult, error) {
  return telemetryFromUsage(callResult?.usage, callResult, error);
}

// emptyTelemetry returns the literal zero-valued row the
// dashboard expects on a failed call (or for any caller
// that needs to construct a row before the SDK responds).
// The shape is owned by `telemetryFromUsage`; this wrapper
// just calls it with all-null inputs. Kept as an export
// for backward compat — experts.mjs and the test suite
// reach for it directly.
export function emptyTelemetry() {
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
export function stripOpenRouterPrefix(model) {
  if (!model) return "";
  return model.startsWith("openrouter/") ? model.slice("openrouter/".length) : model;
}
