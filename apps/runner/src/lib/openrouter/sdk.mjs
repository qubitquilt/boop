// OpenRouter SDK call wrapper.
//
// Wraps the @openrouter/agent SDK's `callModel` in the runner's
// retry / abort / telemetry envelope. The contract is one
// `callOpenRouter` invocation returns one
// `{ text, usage, model, requestId, durationMs, stepCount }` shape.
//
// Three call sites fan in through this module:
//   - the walkthrough (walkthrough.mjs) — no tools, single-shot
//   - the experts (experts.mjs defaultExpert) — tools enabled
//   - the narrator (orchestrator.mjs runOpenCodeSkill) — tools enabled
//
// The walkthrough stays single-shot (no tools, no loop) because it
// is a small structured request that does not benefit from the
// loop and the prompt is shorter than the experts / narrator.
// Experts + narrator get the agent tool set (run_command,
// read_file, git_diff) defined in `../tools.mjs`; the loop is bounded
// by `stopWhen: stepCountIs(STEP_CAP)` so a runaway agent cannot
// eat the Job's 30-min `activeDeadlineSeconds`.

import { OpenRouter, stepCountIs } from "@openrouter/agent";
import { OPENCODE_TIMEOUT_MS } from "../config.mjs";
import { extractUsage } from "./usage.mjs";
import { stampErrorContext } from "./telemetry.mjs";

export const STEP_CAP = 10;

/**
 * Call the OpenRouter Responses API in-process and return the
 * assistant text plus the SDK's reported usage.
 *
 * The SDK swap moved the runner from @openrouter/sdk's
 * `chatSend` (a single chat-completion round-trip) to
 * @openrouter/agent's `callModel` (an OpenResponses request with
 * optional tool auto-execution). The reviewer can hand the model a
 * tool set (see `../tools.mjs`) and the agent loop runs the tools
 * until the model produces a text response or the stop condition
 * fires; this module's contract stays the same — a single
 * `callOpenRouter` invocation returns one `{ text, usage, model,
 * requestId, durationMs, stepCount }` shape.
 *
 * The hard-kill timer is preserved with the same `OPENCODE_TIMEOUT_MS`
 * budget so the Job's 30-min `activeDeadlineSeconds` still has
 * headroom. The timer races the SDK call; on timeout, an
 * `AbortError` is raised and the runner treats it as a clean
 * failure. When `tools` are passed, `stepCountIs(STEP_CAP)` is
 * layered on top as a second budget so a runaway agent cannot
 * eat the per-call wall-clock budget — `STEP_CAP` defaults to 10,
 * which is comfortable for "run tests, read a file, run the
 * diff" without granting enough headroom for an unbounded loop.
 *
 * @param {string} prompt  the boop review prompt (see prompt.mjs buildBoopPrompt)
 * @param {object} deps  { model, env, client, callModel, tools, system, stepCap, AbortControllerCtor, timeoutMs, log, errlog }
 * @returns {Promise<{
 *   text: string,
 *   usage: {
 *     prompt_tokens: number, completion_tokens: number, total_tokens: number,
 *     cost: number, cached_tokens?: number, reasoning_tokens?: number,
 *     cost_prompt_usd?: number, cost_completion_usd?: number,
 *     cost_upstream_usd?: number, is_byok?: boolean,
 *     server_tool_calls_executed?: number, server_tool_calls_requested?: number,
 *     request_id?: string,
 *   },
 *   model: string,
 *   requestId?: string,
 *   durationMs: number,
 *   stepCount: number,
 * }>}
 * @throws when the SDK throws (4xx/5xx/network), when the call is
 *         aborted, or when the response carries no assistant text.
 */
export async function callOpenRouter(prompt, deps = {}) {
  const {
    model,
    env = process.env,
    client: injectedClient,
    callModel: injectedCallModel,
    tools,
    system,
    stepCap = STEP_CAP,
    AbortControllerCtor = globalThis.AbortController,
    timeoutMs = OPENCODE_TIMEOUT_MS,
    log = () => {},
    errlog = () => {},
  } = deps;

  if (!model) {
    throw new Error("callOpenRouter: `model` is required");
  }
  if (!prompt) {
    throw new Error("callOpenRouter: `prompt` is required");
  }

  const apiKey = env.OPENROUTER_API_KEY;
  if (!apiKey) {
    throw new Error("callOpenRouter: OPENROUTER_API_KEY is not set");
  }

  const client = injectedClient ?? new OpenRouter({ apiKey });
  // The default callModel is the agent SDK's instance method bound
  // to the constructed client. Tests inject `deps.callModel` with
  // a fake that returns a ModelResult-shaped object whose
  // getText/getResponse are mockable. Production just binds to
  // client.callModel directly — no transformation layer needed.
  const callModelFn = injectedCallModel || client.callModel.bind(client);

  const controller = new AbortControllerCtor();
  const timer = setTimeout(() => {
    controller.abort();
  }, timeoutMs);
  log("openrouter", "sending completion", {
    model,
    promptBytes: prompt.length,
    timeoutMs,
    toolCount: Array.isArray(tools) ? tools.length : 0,
  });
  // QUB-105: the wall-clock latency per call is now surfaced
  // on the dashboard row. `startedAt` is the moment we hand
  // off to the SDK; `finishedAt` is the moment the response
  // (or error) is in hand. `Date.now()` is sufficient — the
  // runner does not need monotonic time and the dashboard
  // stores duration_ms as a plain integer.
  const startedAt = Date.now();

  // callModel returns a ModelResult synchronously; errors surface
  // on the first access (getText/getResponse) via `throw
  // result.error` inside the SDK. Wrapping the access in a
  // try/catch lets the abort / genuine-error split live in one
  // place. The `signal` rides in RequestOptions (RequestInit.signal
  // is part of the type), so an in-flight HTTP request is cancelled
  // when the timer fires.
  const request = {
    model,
    input: [{ role: "user", content: prompt }],
    ...(system ? { instructions: system } : {}),
    ...(Array.isArray(tools) && tools.length > 0
      ? { tools, stopWhen: stepCountIs(stepCap) }
      : {}),
  };
  const options = { signal: controller.signal };

  try {
    const result = await callModelFn(request, options);
    let text;
    let response;
    try {
      text = await result.getText();
      response = await result.getResponse();
    } catch (err) {
      // QUB-105 / abort split: the SDK surfaces AbortError
      // when the request was cancelled (timeout), and typed
      // errors with statusCode / body / contentType when the
      // API rejected the request. Same split as the
      // orchestrator path; the runner handles AbortError as a
      // clean timeout and any other thrown error as a genuine
      // SDK failure.
      if (err?.name === "AbortError") {
        throw err;
      }
      const wrappedErr = wrapSdkError(err, startedAt);
      errlog("openrouter", "sdk call failed", {
        status: wrappedErr.statusCode,
        // `message` is the SDK's raw error message (without the
        // "(401)" prefix) so the operator's log triage sees the
        // exact string OpenRouter returned. `wrappedMessage`
        // carries the runner-prefixed form for context.
        message: String(err?.message ?? err),
        wrappedMessage: wrappedErr.message,
        errorName: err?.name,
        contentType: wrappedErr.errorContentType,
        body: wrappedErr.errorBody,
        raw: wrappedErr.raw,
        stack: wrappedErr.stackDetail,
      });
      throw wrappedErr;
    }

    if (!text) {
      throw new Error("OpenRouter completion returned no assistant text");
    }

    // QUB-<next: surface the agent loop's actual step count. The
    // agent SDK runs up to STEP_CAP turns; each tool call is a
    // turn, plus the final text response. `result.getToolCalls()`
    // returns the tool invocations the agent executed during the
    // loop (empty when no tools are configured or none fired).
    // stepCount = toolCalls.length + 1 captures every turn the
    // agent took — useful for the dashboard's cost-per-step
    // rollup now that tool-using runs may take 2-N turns.
    // When `getToolCalls` isn't available (older SDK shape, or
    // a thrown error path), stepCount falls back to 1 so the
    // dashboard never sees a missing field.
    let stepCount = 1;
    try {
      if (typeof result.getToolCalls === "function") {
        const toolCalls = await result.getToolCalls();
        stepCount = (Array.isArray(toolCalls) ? toolCalls.length : 0) + 1;
      }
    } catch {
      // getToolCalls may throw on a malformed response; fall
      // back to the single-turn default rather than failing
      // the whole review.
    }

    // QUB-105: the response `id` is OpenRouter's per-request
    // identifier; it lets an operator correlate a dashboard
    // row with the OpenRouter activity log without grepping
    // pod logs. `createdAt` is the unix-second timestamp the
    // SDK stamps on the response; we use it for nothing today
    // but capture it so a future "model time-to-first-token"
    // metric has the source data without another wire change.
    const finishedAt = Date.now();
    return {
      text,
      usage: extractUsage(response),
      model: response?.model || model,
      requestId: typeof response?.id === "string" ? response.id : undefined,
      durationMs: finishedAt - startedAt,
      stepCount,
    };
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Translate a thrown SDK error into the QUB-105-shaped Error the
 * runner expects. The agent SDK surfaces the same OpenRouterError
 * shape the chat-completion client did (statusCode / body /
 * contentType); transport-level failures throw a plain Error.
 * `wrapSdkError` pins every field the runner + dashboard surface.
 */
function wrapSdkError(err, startedAt) {
  const status =
    err && typeof err === "object" && "statusCode" in err
      ? err.statusCode
      : undefined;
  const message =
    err && typeof err === "object" && "message" in err
      ? String(err.message)
      : String(err);
  const body =
    err && typeof err === "object" && "body" in err
      ? String(err.body).slice(0, 500)
      : undefined;
  const contentType =
    err && typeof err === "object" && "contentType" in err
      ? String(err.contentType)
      : undefined;
  const raw = (() => {
    try {
      return JSON.stringify(err, Object.getOwnPropertyNames(err ?? {}));
    } catch {
      return undefined;
    }
  })();
  const stackDetail =
    err && typeof err === "object" && typeof err.stack === "string"
      ? err.stack.split("\n").slice(0, 5).join("\n")
      : undefined;
  const wrappedErr = new Error(
    `OpenRouter completion failed${status ? ` (${status})` : ""}: ${message}`,
  );
  // RF-005: the QUB-105 field set is defined once in
  // `telemetry.mjs` so the read-side (`buildTelemetry`) and
  // this write-side stay in sync. durationMs is the runner's
  // wall-clock for the SDK call; the rest are pass-throughs
  // from the raw SDK error.
  stampErrorContext(
    wrappedErr,
    status,
    contentType,
    body,
    Date.now() - startedAt,
  );
  wrappedErr.raw = raw;
  wrappedErr.stackDetail = stackDetail;
  return wrappedErr;
}

/**
 * Pull the assistant text out of the SDK response.
 *
 * The OpenResponses API returns `output[]` with `message` items
 * whose `content[]` is an array of `{ type: "output_text", text }`
 * parts. The agent SDK concatenates them via `getText()` (which
 * `callOpenRouter` already consumes), so this helper exists for
 * tests that construct raw responses without calling `getText`.
 */
export function extractAssistantText(response) {
  if (!response) return "";
  const output = response.output;
  if (Array.isArray(output)) {
    const parts = [];
    for (const item of output) {
      if (!item || item.type !== "message") continue;
      const content = Array.isArray(item.content) ? item.content : [];
      for (const part of content) {
        if (part && typeof part.text === "string") parts.push(part.text);
      }
    }
    return parts.join("");
  }
  // Legacy chat-completion shape (`choices[0].message.content`),
  // kept so existing test fixtures and the older `response` paths
  // still resolve to a string when a test passes both shapes.
  const message = response?.choices?.[0]?.message;
  if (message && typeof message.content === "string") return message.content;
  if (message && Array.isArray(message.content)) {
    return message.content
      .map((p) => (p && typeof p.text === "string" ? p.text : ""))
      .join("");
  }
  return "";
}
