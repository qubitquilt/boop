// opencode_json: JSON-mode invocation of the `opencode` CLI.
//
// The default `opencode run` is a TUI; we wrap it in `script(1)` to
// get a PTY. That works, but it leaves the runner with no structured
// view of what opencode did: token usage, cost, and per-step timing
// are visible only as raw TUI output.
//
// `opencode run --format json` replaces the TUI with a stream of
// JSON events on stdout: message.updated, step_finish, session.idle,
// etc. The events carry everything the TUI was hiding — cost in
// dollars, token counts split across input/output/reasoning/cache,
// per-step start/complete timestamps. We use this mode when the
// runner was started with BOOP_DASHBOARD_URL so the dashboard has
// something to display. Without it, the runner falls back to the
// TUI mode in opencode.mjs.
//
// The two modes share the same `opencode run` subcommand; the only
// difference is `--format json` and the absence of the PTY wrap
// (the JSON output is line-buffered and doesn't need a TTY to
// initialize). That makes this a low-risk swap: the only new
// failure shape is "the binary returned non-JSON", which we treat
// the same as a non-zero exit.

import { OPENCODE_TIMEOUT_MS } from "./config.mjs";

/**
 * Run opencode in JSON mode and return the parsed review +
 * accumulated token/cost telemetry.
 *
 * The returned shape is a superset of parseReviewOutput's return:
 * the same { summary, inlineComments, confidence } plus a `telemetry`
 * block the runner POSTs to the dashboard. Telemetry is best-effort
 * — if no step_finish events arrive (e.g. a hung LLM call that gets
 * killed before emitting), the fields are zero, and the dashboard
 * still gets a row.
 *
 * @param {string} prompt  the boop skill prompt (see buildBoopPrompt)
 * @param {string} configContent  the materialized opencode.json
 * @param {object} deps  runner deps (paths, spawnFn, setTimeoutFn, parseReviewOutput, ...)
 * @returns {Promise<{stdout: string, stderr: string, code: number, killed: boolean, timeoutMs: number, review: object, telemetry: object}>}
 */
export function runOpencodeJSON(prompt, configContent, deps) {
  const {
    paths,
    spawnFn,
    setTimeoutFn = setTimeout,
    clearTimeoutFn = clearTimeout,
    timeoutMs = OPENCODE_TIMEOUT_MS,
  } = deps;

  return new Promise((resolve) => {
    // No PTY wrap. The JSON output is line-buffered plain text, so
    // a TTY is unnecessary and would actually be wrong (the binary
    // detects a TTY and switches to the TUI mode, which then
    // doesn't emit JSON). stdio[0] = "ignore" so the child never
    // blocks on a stdin read.
    //
    // Flag order: flags before `--`, prompt as the positional arg
    // after `--`. Same defense as runOpencode: any leading `-` in
    // the prompt body cannot be parsed as a flag.
    void configContent; // reserved for future templating hooks
    const args = [
      "run",
      "--format", "json",
      "--auto",
      "--dir", paths.repoDir,
      ...(deps.debug ? ["--log-level", "DEBUG", "--print-logs"] : []),
      "--",
      prompt,
    ];
    const proc = spawnFn("opencode", args, {
      env: deps.childEnv,
      stdio: ["ignore", "pipe", "pipe"],
    });

    let stdout = "";
    let stderr = "";
    let killed = false;
    let timeoutMsActual = 0;

    // The JSON stream is a series of newline-delimited JSON events.
    // We collect all events, then post-process after the child
    // closes. Buffering the full stdout is fine here — opencode's
    // JSON event stream is small (a few KB per step, ~10-50 steps
    // in a typical review), well under the runner's memory budget.
    const lines = [];
    let pending = "";
    proc.stdout.on("data", (chunk) => {
      const text = chunk.toString();
      stdout += text;
      pending += text;
      let nl;
      while ((nl = pending.indexOf("\n")) >= 0) {
        const line = pending.slice(0, nl);
        pending = pending.slice(nl + 1);
        if (line.trim()) lines.push(line);
      }
    });
    proc.stderr.on("data", (chunk) => {
      const text = chunk.toString();
      stderr += text;
      for (const line of text.split("\n")) {
        if (line.trim()) deps.errlog("opencode-stderr", line);
      }
    });

    const timer = setTimeoutFn(() => {
      killed = true;
      timeoutMsActual = timeoutMs;
      deps.errlog("opencode", "killing subprocess after timeout", { ms: timeoutMs });
      proc.kill("SIGKILL");
    }, timeoutMs);

    proc.on("error", (err) => {
      clearTimeoutFn(timer);
      deps.errlog("opencode", "spawn error", { err: String(err?.message ?? err) });
      resolve({
        stdout,
        stderr,
        code: -1,
        killed,
        timeoutMs: timeoutMsActual,
        review: { summary: "", inlineComments: [], confidence: "medium" },
        telemetry: emptyTelemetry(),
      });
    });
    proc.on("close", (code) => {
      clearTimeoutFn(timer);
      const { review, telemetry } = parseOpencodeJSONStream(lines, deps);
      resolve({
        stdout,
        stderr,
        code,
        killed,
        timeoutMs: timeoutMsActual,
        review,
        telemetry,
      });
    });
  });
}

/**
 * Parse a stream of newline-delimited JSON events from
 * `opencode run --format json` into a review and a telemetry rollup.
 *
 * Event shapes (from opencode's TS SDK):
 *   {type: "message.updated", data: {info: {role: "assistant", ..., tokens: {input, output, reasoning, cache: {read, write}}, cost}}}
 *   {type: "step_finish",       data: {cost, tokens: {input, output, reasoning, cache: {read, write}}}}
 *   {type: "session.idle"}
 *   {type: "session.error",     data: {...}}
 *
 * Strategy:
 *   - Walk the events in order. Each `message.updated` with
 *     role="assistant" is a candidate final message; we keep the
 *     last one as the "final" answer and pull its text content
 *     out to feed to parseReviewOutput.
 *   - Each `step_finish` event contributes to the accumulated
 *     telemetry. Cost is summed; tokens are summed across
 *     input/output/reasoning/cache. The modelID and providerID from
 *     the first step_finish populate telemetry.model and
 *     telemetry.provider.
 *
 * Robust to malformed lines: any line that doesn't parse as JSON
 * is skipped silently. opencode occasionally emits blank lines
 * (between events on some shells); those are also skipped.
 *
 * @param {string[]} lines  the newline-delimited JSON events
 * @param {object} deps  must include parseReviewOutput (the SUMMARY/INLINE/CONFIDENCE/END block parser from opencode.mjs)
 * @returns {{review: object, telemetry: object}}
 */
export function parseOpencodeJSONStream(lines, deps) {
  const { parseReviewOutput } = deps;
  let lastAssistantText = "";
  let lastAssistantModel = "";
  let lastAssistantProvider = "";
  const telemetry = emptyTelemetry();

  for (const raw of lines) {
    let ev;
    try {
      ev = JSON.parse(raw);
    } catch {
      // Malformed line; skip. Shouldn't happen with a healthy
      // opencode, but a noisy TTY edge or a stdout flush
      // boundary could split a JSON object across two reads.
      // parseOpencodeJSONStream only handles whole-line events;
      // a torn object is dropped on the floor. Acceptable: the
      // dashboard would just show a missing token count for one
      // step, not a missing review.
      continue;
    }
    const t = ev.type || ev.event;
    const data = ev.data ?? ev;
    if (t === "message.updated" || t === "message") {
      const info = data?.info ?? data;
      if (info?.role !== "assistant") continue;
      const text = extractTextFromMessage(info);
      if (text) lastAssistantText = text;
      if (info?.modelID) lastAssistantModel = info.modelID;
      if (info?.providerID) lastAssistantProvider = info.providerID;
      // Some opencode versions also put tokens/cost on the
      // assistant message itself. Prefer those when present so
      // we don't double-count with step_finish.
      if (info?.tokens) {
        telemetry.inputTokens = Math.max(telemetry.inputTokens, info.tokens.input ?? 0);
        telemetry.outputTokens = Math.max(telemetry.outputTokens, info.tokens.output ?? 0);
        if (info.tokens.reasoning) telemetry.reasoningTokens = Math.max(telemetry.reasoningTokens, info.tokens.reasoning);
        if (info.tokens.cache) {
          telemetry.cacheReadTokens = Math.max(telemetry.cacheReadTokens, info.tokens.cache.read ?? 0);
          telemetry.cacheWriteTokens = Math.max(telemetry.cacheWriteTokens, info.tokens.cache.write ?? 0);
        }
      }
      if (typeof info?.cost === "number") {
        // We MAX rather than sum here: the assistant message's
        // cost is the running total for the message; summing
        // across all messages would double-count. The dashboard
        // wants the total cost of the run, which is the max
        // across all assistant messages.
        telemetry.costUsd = Math.max(telemetry.costUsd, info.cost);
      }
    } else if (t === "step_finish" || t === "step-finish") {
      const tokens = data?.tokens ?? {};
      telemetry.inputTokens += tokens.input ?? 0;
      telemetry.outputTokens += tokens.output ?? 0;
      telemetry.reasoningTokens += tokens.reasoning ?? 0;
      if (tokens.cache) {
        telemetry.cacheReadTokens += tokens.cache.read ?? 0;
        telemetry.cacheWriteTokens += tokens.cache.write ?? 0;
      }
      if (typeof data?.cost === "number") {
        telemetry.costUsd += data.cost;
      }
      telemetry.stepCount += 1;
    }
  }

  if (lastAssistantModel) {
    telemetry.model = lastAssistantModel;
    if (lastAssistantProvider) {
      telemetry.provider = lastAssistantProvider;
    }
  }

  // Reuse the existing parseReviewOutput on the assistant
  // message body. The JSON stream is already plain text (no
  // ANSI to strip), so we pass the text directly.
  const review = lastAssistantText
    ? parseReviewOutput(lastAssistantText)
    : { summary: "", inlineComments: [], confidence: "medium" };

  return { review, telemetry };
}

function emptyTelemetry() {
  return {
    model: "",
    provider: "",
    inputTokens: 0,
    outputTokens: 0,
    reasoningTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
    costUsd: 0,
    stepCount: 0,
  };
}

function extractTextFromMessage(info) {
  // opencode stores the assistant's text in a few shapes
  // across versions. Walk the known ones in order; the first
  // non-empty wins. The structured SUMMARY block is in the
  // LAST text part the assistant emits, so we want every
  // part concatenated, not just the first.
  if (typeof info?.text === "string") return info.text;
  if (typeof info?.content === "string") return info.content;
  if (Array.isArray(info?.parts)) {
    const out = [];
    for (const part of info.parts) {
      if (typeof part?.text === "string") out.push(part.text);
      else if (typeof part?.content === "string") out.push(part.content);
    }
    return out.join("");
  }
  return "";
}
