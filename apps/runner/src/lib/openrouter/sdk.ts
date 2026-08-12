// OpenRouter SDK call wrapper.
//
// Wraps the @openrouter/agent SDK's `callModel` in the runner's
// retry / abort / telemetry envelope. The contract is one
// `callOpenRouter` invocation returns one
// `{ text, usage, model, requestId, durationMs, stepCount }` shape.
//
// Three call sites fan in through this module:
//   - the walkthrough (walkthrough.ts) — no tools, single-shot
//   - the experts (experts.ts defaultExpert) — tools enabled
//   - the narrator (orchestrator.ts runOpenCodeSkill) — tools enabled
//
// The walkthrough stays single-shot (no tools, no loop) because it
// is a small structured request that does not benefit from the
// loop and the prompt is shorter than the experts / narrator.
// Experts + narrator get the agent tool set (run_command,
// read_file, git_diff) defined in `../tools.ts`; the loop is bounded
// by `stopWhen: stepCountIs(STEP_CAP)` so a runaway agent cannot
// eat the Job's 30-min `activeDeadlineSeconds`.

import { OpenRouter, stepCountIs, callModel } from "@openrouter/agent";
import { OPENCODE_TIMEOUT_MS } from "../config.ts";
import { extractUsage } from "./usage.ts";
import { stampErrorContext } from "./telemetry.ts";
import type { CallDeps, CallResult, ChatResult, ModelResult } from "../../types.ts";

export const STEP_CAP = 10;

type AnyTool = Parameters<typeof stepCountIs>[0] extends never ? never : unknown;

export async function callOpenRouter(
  prompt: string,
  deps: CallDeps = {} as CallDeps,
): Promise<CallResult> {
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
  // The standalone `callModel` export from `@openrouter/agent`
  // has signature `(client, request, options)` — the OpenRouter
  // instance is the first argument and the SDK then pulls
  // per-call config (timeout, baseURL, ...) off `client`. The
  // agent SDK exposes the same function as a method on the
  // OpenRouter class so most call sites write
  // `client.callModel(request)`. Production here uses the
  // standalone import (so a future refactor that swaps the
  // method for a free function does not have to touch this
  // file); we pass `client` explicitly. A previous version of
  // this code used `client.callModel.bind(client)` — JS does
  // not auto-bind across non-method calls, so the SDK crashed
  // on the first dispatch with `undefined is not an object
  // (evaluating 'this.options.client._options.timeoutMs')`.
  // The local-run smoke test against a real PR caught it
  // because the unit tests inject `deps.callOpenRouter` and
  // bypass the SDK entirely.
  const callModelFn =
    injectedCallModel ||
    ((req: unknown, opts: unknown) =>
      (callModel as unknown as (c: unknown, r: unknown, o: unknown) => Promise<ModelResult>)(
        client,
        req,
        opts,
      ));

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
  const startedAt = Date.now();

  const request = {
    model,
    input: [{ role: "user", content: prompt }],
    ...(system ? { instructions: system } : {}),
    ...(Array.isArray(tools) && tools.length > 0
      ? { tools, stopWhen: stepCountIs(stepCap) as AnyTool }
      : {}),
  };
  const options = { signal: controller.signal };

  try {
    const result: ModelResult = await callModelFn(request, options);
    let text: string;
    let response: ChatResult;
    try {
      text = await result.getText();
      response = await result.getResponse();
    } catch (err) {
      if (err && typeof err === "object" && "name" in err && (err as { name: string }).name === "AbortError") {
        throw err;
      }
      const wrappedErr = wrapSdkError(err, startedAt);
      errlog("openrouter", "sdk call failed", {
        status: wrappedErr.statusCode,
        message: String((err as { message?: unknown })?.message ?? err),
        wrappedMessage: wrappedErr.message,
        errorName: (err as { name?: string })?.name,
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

    let stepCount = 1;
    try {
      if (typeof result.getToolCalls === "function") {
        const toolCalls = await result.getToolCalls();
        stepCount = (Array.isArray(toolCalls) ? toolCalls.length : 0) + 1;
      }
    } catch {
      // fall back to 1
    }

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

function wrapSdkError(err: unknown, startedAt: number): Error & {
  statusCode?: number;
  errorContentType?: string;
  errorBody?: string;
  durationMs?: number;
  raw?: string;
  stackDetail?: string;
} {
  const e = err as { statusCode?: number; message?: unknown; body?: unknown; contentType?: unknown; stack?: string };
  const status = e && typeof e === "object" && "statusCode" in e ? e.statusCode : undefined;
  const message = e && typeof e === "object" && "message" in e ? String(e.message) : String(err);
  const body = e && typeof e === "object" && "body" in e ? String(e.body).slice(0, 500) : undefined;
  const contentType =
    e && typeof e === "object" && "contentType" in e ? String(e.contentType) : undefined;
  const raw = (() => {
    try {
      return JSON.stringify(err, Object.getOwnPropertyNames(err as object));
    } catch {
      return undefined;
    }
  })();
  const stackDetail =
    e && typeof e === "object" && typeof e.stack === "string"
      ? e.stack.split("\n").slice(0, 5).join("\n")
      : undefined;
  const wrappedErr = new Error(
    `OpenRouter completion failed${status ? ` (${status})` : ""}: ${message}`,
  ) as Error & {
    statusCode?: number;
    errorContentType?: string;
    errorBody?: string;
    durationMs?: number;
    raw?: string;
    stackDetail?: string;
  };
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
export function extractAssistantText(response: ChatResult | null | undefined): string {
  if (!response) return "";
  const output = response.output;
  if (Array.isArray(output)) {
    const parts: string[] = [];
    for (const item of output) {
      if (!item || typeof item !== "object" || (item as { type?: string }).type !== "message") continue;
      const content = (item as { content?: unknown }).content;
      if (!Array.isArray(content)) continue;
      for (const part of content) {
        if (part && typeof part === "object" && typeof (part as { text?: unknown }).text === "string") {
          parts.push((part as { text: string }).text);
        }
      }
    }
    return parts.join("");
  }
  const message = (response as { choices?: Array<{ message?: { content?: unknown } }> })
    ?.choices?.[0]?.message;
  if (message && typeof message.content === "string") return message.content;
  if (message && Array.isArray(message.content)) {
    return message.content
      .map((p) => (p && typeof p === "object" && typeof (p as { text?: unknown }).text === "string" ? (p as { text: string }).text : ""))
      .join("");
  }
  return "";
}

// re-export callModel so a future test seam can swap a fake for
// the SDK's exported function. The runner does not call this
// directly today; the export keeps the surface name around for
// backward compat with the pre-cutover tests.
export { callModel };
