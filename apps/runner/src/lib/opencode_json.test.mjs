import { test } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";

import { parseOpencodeJSONStream, runOpencodeJSON } from "./opencode_json.mjs";
import { parseReviewOutput } from "./opencode.mjs";

const parseDeps = { parseReviewOutput };

function makeStreamEvent(type, data) {
  return JSON.stringify({ type, data });
}

test("parseOpencodeJSONStream extracts last assistant message and parses structured block", () => {
  const lines = [
    makeStreamEvent("session.created", { id: "abc" }),
    makeStreamEvent("message.updated", {
      info: { role: "user", text: "ignored" },
    }),
    makeStreamEvent("message.updated", {
      info: {
        role: "assistant",
        modelID: "openrouter/anthropic/claude-3.5-sonnet",
        providerID: "openrouter",
        text: "ignored intermediate",
      },
    }),
    makeStreamEvent("message.updated", {
      info: {
        role: "assistant",
        modelID: "openrouter/anthropic/claude-3.5-sonnet",
        providerID: "openrouter",
        text:
          "=== SUMMARY ===\n" +
          "## TL;DR\nLooks fine.\n" +
          "=== INLINE COMMENTS ===\n" +
          "src/foo.ts:10: heads up\n" +
          "=== CONFIDENCE ===\n" +
          "high\n" +
          "=== END ===\n",
      },
    }),
    makeStreamEvent("session.idle", {}),
  ];
  const { review, telemetry } = parseOpencodeJSONStream(lines, parseDeps);
  assert.equal(review.summary, "## TL;DR\nLooks fine.");
  assert.equal(review.confidence, "high");
  assert.equal(review.inlineComments.length, 1);
  assert.equal(review.inlineComments[0].path, "src/foo.ts");
  assert.equal(telemetry.model, "openrouter/anthropic/claude-3.5-sonnet");
  assert.equal(telemetry.provider, "openrouter");
});

test("parseOpencodeJSONStream aggregates step_finish token + cost", () => {
  const lines = [
    makeStreamEvent("step_finish", {
      tokens: { input: 100, output: 50, reasoning: 0, cache: { read: 0, write: 0 } },
      cost: 0.001,
    }),
    makeStreamEvent("step_finish", {
      tokens: { input: 200, output: 75, reasoning: 10, cache: { read: 50, write: 0 } },
      cost: 0.002,
    }),
  ];
  const { telemetry } = parseOpencodeJSONStream(lines, parseDeps);
  assert.equal(telemetry.inputTokens, 300);
  assert.equal(telemetry.outputTokens, 125);
  assert.equal(telemetry.reasoningTokens, 10);
  assert.equal(telemetry.cacheReadTokens, 50);
  assert.equal(telemetry.costUsd, 0.003);
  assert.equal(telemetry.stepCount, 2);
});

test("parseOpencodeJSONStream prefers message-level cost over step_finish sum (avoids double count)", () => {
  // opencode reports cost on each message as a running total;
  // summing across messages would over-count. We MAX.
  const lines = [
    makeStreamEvent("message.updated", {
      info: { role: "assistant", cost: 0.01, text: "first chunk" },
    }),
    makeStreamEvent("message.updated", {
      info: { role: "assistant", cost: 0.03, text: "more text" },
    }),
  ];
  const { telemetry } = parseOpencodeJSONStream(lines, parseDeps);
  assert.equal(telemetry.costUsd, 0.03);
});

test("parseOpencodeJSONStream returns empty telemetry on empty stream", () => {
  const { review, telemetry } = parseOpencodeJSONStream([], parseDeps);
  assert.equal(review.summary, "");
  assert.equal(telemetry.costUsd, 0);
  assert.equal(telemetry.model, "");
});

test("parseOpencodeJSONStream skips malformed JSON lines", () => {
  const lines = [
    "{not json}",
    makeStreamEvent("message.updated", { info: { role: "assistant", text: "ok" } }),
    "also not json",
  ];
  const { review } = parseOpencodeJSONStream(lines, parseDeps);
  // No structured block in "ok" — fallback shape.
  assert.equal(review.summary, "ok");
});

test("parseOpencodeJSONStream concatenates text parts", () => {
  const lines = [
    makeStreamEvent("message.updated", {
      info: {
        role: "assistant",
        parts: [
          { text: "part one " },
          { text: "part two" },
        ],
      },
    }),
  ];
  const { review } = parseOpencodeJSONStream(lines, parseDeps);
  assert.equal(review.summary, "part one part two");
});

test("runOpencodeJSON resolves with parsed review + telemetry", async () => {
  const proc = new EventEmitter();
  proc.stdout = new EventEmitter();
  proc.stderr = new EventEmitter();
  proc.kill = () => {};

  const spawnFn = () => proc;
  const lines = [
    makeStreamEvent("message.updated", {
      info: {
        role: "assistant",
        modelID: "m1",
        providerID: "p1",
        text: "=== SUMMARY ===\nbody\n=== INLINE COMMENTS ===\n=== CONFIDENCE ===\nlow\n=== END ===\n",
      },
    }),
    makeStreamEvent("step_finish", {
      tokens: { input: 10, output: 5 },
      cost: 0.0001,
    }),
  ];

  const promise = runOpencodeJSON("prompt", "{}", {
    paths: { repoDir: "/work/repo" },
    spawnFn,
    debug: false,
    childEnv: {},
    log: () => {},
    errlog: () => {},
    parseReviewOutput,
  });

  // Emit data then close.
  for (const l of lines) {
    proc.stdout.emit("data", l + "\n");
  }
  setImmediate(() => proc.emit("close", 0));

  const result = await promise;
  assert.equal(result.code, 0);
  assert.equal(result.telemetry.costUsd, 0.0001);
  assert.equal(result.telemetry.inputTokens, 10);
  assert.equal(result.review.confidence, "low");
  assert.equal(result.review.summary, "body");
});

test("runOpencodeJSON returns empty telemetry on spawn error", async () => {
  const proc = new EventEmitter();
  proc.stdout = new EventEmitter();
  proc.stderr = new EventEmitter();
  proc.kill = () => {};

  const spawnFn = () => proc;

  const promise = runOpencodeJSON("prompt", "{}", {
    paths: { repoDir: "/work/repo" },
    spawnFn,
    debug: false,
    childEnv: {},
    log: () => {},
    errlog: () => {},
    parseReviewOutput,
  });

  setImmediate(() => proc.emit("error", new Error("ENOENT")));
  const result = await promise;
  assert.equal(result.code, -1);
  assert.equal(result.telemetry.costUsd, 0);
  assert.equal(result.review.summary, "");
});
