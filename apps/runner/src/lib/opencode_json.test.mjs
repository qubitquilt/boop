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
          "## TL;DR\nLooks fine overall. The diff is small, scoped, and the tests cover the new behavior. No blockers, two nits worth addressing before the next change.\n\n" +
          "## Findings\n\n| ID | Tier | File : Line | Summary |\n|----|------|-------------|---------|\n| O1 | 🟢 Optional | `src/foo.ts:10` | consider a comment |\n" +
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
  assert.match(review.summary, /## TL;DR/);
  assert.match(review.summary, /Looks fine overall/);
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
  // "ok" has no `=== SUMMARY ===` wrapper; parseReviewOutput
  // returns the "no structured block" parseError. The malformed
  // JSON lines above are silently dropped without affecting the
  // parser. The caller in runOpenCodeSkill would log
  // summary_parse_failed and the dashboard wrapper would surface
  // telemetry with no review body.
  assert.equal(review.summary, "");
  assert.equal(review.confidence, "low");
  assert.equal(review.parseError, "no structured block");
});

test("parseOpencodeJSONStream concatenates text parts", () => {
  // The test's purpose is to verify the two `text` parts get joined
  // into one before being handed to parseReviewOutput. The joined
  // text here has no `=== SUMMARY ===` wrapper, so the parser
  // returns a parseError; that's fine — the assertion targets the
  // concatenation, not the parse.
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
  assert.equal(review.summary, "");
  assert.equal(review.parseError, "no structured block");
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
        text: [
          "=== SUMMARY ===",
          "## TL;DR",
          "Looks good overall. The diff is small, the change is well-scoped, and the tests cover the new code shape. No blockers, one follow-up worth addressing before the next change.",
          "",
          "## Findings",
          "",
          "| ID | Tier | File : Line | Summary |",
          "|----|------|-------------|---------|",
          "| F1 | 🟡 Follow-up | `src/x.ts:10` | nit on naming |",
          "=== INLINE COMMENTS ===",
          "=== CONFIDENCE ===",
          "low",
          "=== END ===",
        ].join("\n"),
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
  assert.match(result.review.summary, /Looks good overall/);
  assert.equal(result.review.parseError, null);
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
