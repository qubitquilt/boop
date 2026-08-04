// OpenRouter SDK pipeline.
//
// The runner used to shell out to the `opencode` CLI wrapped in
// `script(1)` for a PTY. The CLI hung at init in non-TTY environments
// and surfaced token / cost telemetry only as raw TUI output, which a
// second JSON-mode parser had to roll up.
//
// This module is the in-process replacement: one non-streaming chat
// completion against the OpenRouter SDK. The runner still builds the
// boop prompt (see `buildBoopPrompt` in `./opencode.mjs`) — only the
// invocation mechanism changes. The API key reaches the SDK via
// `process.env.OPENROUTER_API_KEY`; no `opencode.json` template, no
// subprocess, no PTY wrap.
//
// Telemetry comes straight from the SDK response's `usage` block:
// `prompt_tokens` and `completion_tokens` map onto `inputTokens` and
// `outputTokens`; `cost` (when the SDK exposes it) becomes `costUsd`.
// `model` comes from the response. `provider` is dropped — OpenRouter
// is the single provider now, so the field is dead weight.

import { OpenRouter } from "@openrouter/sdk";
import { OPENCODE_TIMEOUT_MS } from "./config.mjs";

/**
 * Call the OpenRouter chat completion API in-process and return the
 * assistant text plus the SDK's reported usage.
 *
 * The function is deliberately minimal: one user message, no tools,
 * no streaming. The boop review is a single-shot prompt; tool use
 * belongs in a follow-up that re-uses the boop lenses as explicit
 * tool calls.
 *
 * The hard-kill timer is preserved (with the same `OPENCODE_TIMEOUT_MS`
 * budget) so the Job's 30-min `activeDeadlineSeconds` still has
 * headroom. The timer races the SDK call; on timeout, an `AbortError`
 * is raised and the runner treats it as a clean failure.
 *
 * @param {string} prompt  the boop review prompt (see buildBoopPrompt)
 * @param {object} deps  { model, env, client, AbortControllerCtor, timeoutMs, log }
 * @returns {Promise<{ text: string, usage: { prompt_tokens: number, completion_tokens: number, cost: number, cached_tokens?: number, reasoning_tokens?: number }, model: string }>}
 * @throws when the SDK returns a non-ok result, when the call is aborted,
 *         or when the response carries no assistant text.
 */
export async function callOpenRouter(prompt, deps = {}) {
  const {
    model,
    env = process.env,
    client: injectedClient,
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
  const controller = new AbortControllerCtor();
  const timer = setTimeout(() => {
    controller.abort();
  }, timeoutMs);
  log("openrouter", "sending chat completion", {
    model,
    promptBytes: prompt.length,
    timeoutMs,
  });

  try {
    const result = await client.chat.send(
      {
        chatRequest: {
          model,
          messages: [{ role: "user", content: prompt }],
          stream: false,
        },
      },
      { abortSignal: controller.signal },
    );

    if (!result.ok) {
      const err = result.error;
      const status =
        err && typeof err === "object" && "statusCode" in err
          ? err.statusCode
          : undefined;
      const message = err && typeof err === "object" && "message" in err
        ? String(err.message)
        : String(err);
      // Surface the SDK's own failure in the error pipeline so a
      // 4xx/5xx doesn't look like a successful empty review in
      // the log. The throw below is the contract the runner
      // expects; errlog is an additional breadcrumb.
      errlog("openrouter", "sdk returned non-ok result", {
        status,
        message,
      });
      throw new Error(
        `OpenRouter chat completion failed${status ? ` (${status})` : ""}: ${message}`,
      );
    }

    const response = result.value;
    const text = extractAssistantText(response);
    if (!text) {
      throw new Error("OpenRouter chat completion returned no assistant text");
    }

    return {
      text,
      usage: extractUsage(response),
      model: response.model || model,
    };
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Pull the assistant text out of the SDK response.
 *
 * The SDK returns `choices[0].message.content` as either a string or
 * an array of `ChatContentItems` (when the model emits structured
 * content). The boop review is a single text block, so a non-empty
 * string is the common case; the array form is supported for
 * forward-compatibility.
 */
function extractAssistantText(response) {
  const message = response?.choices?.[0]?.message;
  if (!message) return "";
  const content = message.content;
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    const out = [];
    for (const part of content) {
      if (typeof part?.text === "string") out.push(part.text);
    }
    return out.join("");
  }
  return "";
}

/**
 * Map the SDK `usage` object onto the runner's telemetry shape.
 *
 * - `prompt_tokens` → `prompt_tokens` (runner field name)
 * - `completion_tokens` → `completion_tokens`
 * - `cost` (when present) → `cost`. Missing → 0 so the dashboard
 *   row still gets a numeric value.
 * - Cached / reasoning tokens surface when the SDK reports them;
 *   the dashboard can ignore fields it doesn't render.
 *
 * The runner's telemetry contract (see `postTelemetry` in
 * `./dashboard.mjs`) expects the snake_case keys below; the field
 * rename to `inputTokens` / `outputTokens` happens in the caller
 * (`buildTelemetry`).
 */
function extractUsage(response) {
  const usage = response?.usage;
  if (!usage || typeof usage !== "object") {
    return {
      prompt_tokens: 0,
      completion_tokens: 0,
      cost: 0,
    };
  }
  // Omit optional fields (cached_tokens, reasoning_tokens) when
  // the SDK doesn't surface them. Returning `{ foo: undefined }`
  // is semantically equivalent to `{}` but breaks deep-equal
  // assertions and serialises to a `null` field in JSON.
  const out = {
    prompt_tokens: numOrZero(usage.promptTokens),
    completion_tokens: numOrZero(usage.completionTokens),
    cost: typeof usage.cost === "number" ? usage.cost : 0,
  };
  const cached = usage.promptTokensDetails?.cachedTokens;
  if (cached != null) {
    out.cached_tokens = numOrZero(cached);
  }
  const reasoning = usage.completionTokensDetails?.reasoningTokens;
  if (reasoning != null) {
    out.reasoning_tokens = numOrZero(reasoning);
  }
  return out;
}

function numOrZero(v) {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/**
 * Build the runner's telemetry object from an OpenRouter call
 * result. This is the shape the runner POSTs to the dashboard —
 * the field names are the existing contract and must not change.
 *
 * - `model` from the response (falls back to the requested model).
 * - `provider` is hard-wired to "openrouter" because that's the
 *   single provider now; the legacy `parseOpencodeJSONStream` shape
 *   carried a `provider` field and the dashboard can render it
 *   without a schema change.
 * - `inputTokens` / `outputTokens` come from the SDK `usage` object.
 * - `costUsd` from the SDK `usage.cost` (0 when missing).
 * - `stepCount` is always 1 — the SDK does one round-trip, vs. the
 *   opencode TUI which could take many steps. Keeping the field
 *   non-null avoids a dashboard-side null check.
 *
 * Returns the empty telemetry object when the call failed before
 * the response landed (timeout, 4xx/5xx, etc.) so the dashboard
 * still gets a row.
 */
export function buildTelemetry(callResult, error) {
  const empty = emptyTelemetry();
  if (!callResult) {
    // Distinguish a failed SDK call from a successful call that
    // happened to produce an empty summary. The dashboard can
    // filter on `error` to separate "model said nothing useful"
    // from "the API rejected the request". `error` is a short
    // string (truncated by the runner's stderr tail logic) and
    // intentionally NOT counted as telemetry — the cost / token
    // fields stay zero so the failure doesn't double-count if
    // the dashboard later sums across runs.
    if (error) empty.error = String(error?.message ?? error);
    return empty;
  }
  return {
    model: callResult.model || "",
    provider: "openrouter",
    inputTokens: callResult.usage?.prompt_tokens ?? 0,
    outputTokens: callResult.usage?.completion_tokens ?? 0,
    reasoningTokens: callResult.usage?.reasoning_tokens ?? 0,
    cacheReadTokens: callResult.usage?.cached_tokens ?? 0,
    cacheWriteTokens: 0,
    costUsd: callResult.usage?.cost ?? 0,
    stepCount: 1,
  };
}

export function emptyTelemetry() {
  return {
    model: "",
    provider: "openrouter",
    inputTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
    costUsd: 0,
    stepCount: 0,
    // `error` is stamped by `buildTelemetry` when the SDK call
    // failed before the response landed. Absent on a successful
    // call (even one whose summary is empty) so the dashboard can
    // tell "model said nothing useful" from "the API rejected
    // the request". The field is intentionally `undefined` here
    // so successful rows don't carry a stale error string.
  };
}

/**
 * Read the model name from the opencode.json ConfigMap mount.
 *
 * During the QUB-94 cutover, the opencode.json ConfigMap is still
 * mounted (its deletion is QUB-98). The SDK path needs a model
 * name; the simplest source of truth is the same `model` field the
 * old `materializeConfig` reads. After QUB-98 the ConfigMap goes
 * away and the runner will fall back to `deps.model` (passed
 * explicitly) or an env override.
 *
 * The read goes straight at the read-only mount (CONFIG_SRC), not
 * the materialized copy — same reason as `buildBoopPrompt`:
 * `cp -rL` on the `..data` symlink can pull prior ConfigMap
 * versions and OOM the container.
 *
 * @param {object} deps  { fs, paths }
 * @returns {Promise<string>} the model name, or "" if the file is missing
 */
export async function readOpencodeModel(deps) {
  const { fs, paths } = deps;
  if (!paths?.configSrc) return "";
  const configPath = `${paths.configSrc}/opencode.json`;
  try {
    const raw = await fs.readFile(configPath, "utf8");
    const parsed = JSON.parse(raw);
    return typeof parsed?.model === "string" ? parsed.model : "";
  } catch {
    return "";
  }
}
