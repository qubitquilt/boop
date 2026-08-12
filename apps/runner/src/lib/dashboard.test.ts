import { test } from "node:test";
import assert from "node:assert/strict";

import { postStatus, postTelemetry, postStage, postLensTelemetry, startHeartbeat } from "./dashboard.ts";

// Captures the last call into a global so individual tests can
// assert on the URL/method/body the helper would have sent.
const sent = [];
function makeFetchOK() {
  return async (url, init) => {
    sent.push({ url, init });
    return { status: 204, text: async () => "" };
  };
}
function makeFetchRetry(thenStatus) {
  let calls = 0;
  return {
    fetch: async (url, init) => {
      calls++;
      sent.push({ url, init });
      if (calls < thenStatus.length) {
        return { status: thenStatus[calls - 1], text: async () => "" };
      }
      return { status: thenStatus[calls - 1], text: async () => "" };
    },
    calls: () => calls,
  };
}

function makeCtx(overrides = {}) {
  return {
    dashboardUrl: "http://boop-receiver:8080",
    dashboardToken: "secret",
    jobName: "boop-a-b-1-aaaaaaa",
    prOwner: "a",
    prRepo: "b",
    prNumber: 1,
    prHeadSha: "aaaaaaa",
    ...overrides,
  };
}

test("postStatus is a no-op when dashboardUrl is unset", async () => {
  sent.length = 0;
  await postStatus("done", { dashboardUrl: null, dashboardToken: "x", jobName: "j" }, { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 0, "no fetch should fire when URL is unset");
});

test("postStatus is a no-op when dashboardToken is unset", async () => {
  sent.length = 0;
  await postStatus("done", { dashboardUrl: "http://x", dashboardToken: null, jobName: "j" }, { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 0);
});

test("postStatus POSTs to /api/runs/:id/status with the right headers", async () => {
  sent.length = 0;
  await postStatus("done", makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 1);
  assert.equal(sent[0].url, "http://boop-receiver:8080/api/runs/boop-a-b-1-aaaaaaa/status");
  assert.equal(sent[0].init.method, "POST");
  assert.equal(sent[0].init.headers["X-BOOP-Runner-Token"], "secret");
  assert.equal(sent[0].init.headers["Content-Type"], "application/json");
  assert.equal(sent[0].init.body, JSON.stringify({ stage: "done" }));
});

test("postTelemetry POSTs the full telemetry payload", async () => {
  sent.length = 0;
  const telem = {
    model: "openrouter/x",
    provider: "openrouter",
    inputTokens: 100,
    outputTokens: 50,
    reasoningTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
    costUsd: 0.005,
    stepCount: 2,
  };
  await postTelemetry(telem, makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 1);
  const body = JSON.parse(sent[0].init.body);
  assert.equal(body.model, "openrouter/x");
  assert.equal(body.input_tokens, 100);
  assert.equal(body.output_tokens, 50);
  assert.equal(body.cost_usd, 0.005);
  assert.equal(body.step_count, 2);
});

test("postTelemetry forwards QUB-105 fields to the dashboard (QUB-105)", async () => {
  // QUB-105 acceptance: the receiver's telemetryRequest struct
  // grows new optional fields. The runner serialises them
  // here; the receiver stores them; the dashboard renders
  // them. The fixture exercises every QUB-105 field including
  // the failed-call error context.
  sent.length = 0;
  const telem = {
    model: "openrouter/anthropic/claude-3.5-sonnet",
    provider: "openrouter",
    inputTokens: 100,
    outputTokens: 50,
    totalTokens: 175,
    reasoningTokens: 10,
    cacheReadTokens: 5,
    cacheWriteTokens: 0,
    costUsd: 0.0123,
    costPromptUsd: 0.001,
    costCompletionUsd: 0.0113,
    costUpstreamUsd: 0.0124,
    isByok: true,
    serverToolCallsExecuted: 0,
    serverToolCallsRequested: 0,
    requestId: "chatcmpl-xyz",
    durationMs: 4321,
    stepCount: 1,
    error: "401 Unauthorized",
    errorStatusCode: 401,
    errorContentType: "application/json",
  };
  await postTelemetry(telem, makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 1);
  const body = JSON.parse(sent[0].init.body);
  assert.equal(body.total_tokens, 175);
  assert.equal(body.cost_prompt_usd, 0.001);
  assert.equal(body.cost_completion_usd, 0.0113);
  assert.equal(body.cost_upstream_usd, 0.0124);
  assert.equal(body.is_byok, true);
  assert.equal(body.server_tool_calls_executed, 0);
  assert.equal(body.server_tool_calls_requested, 0);
  assert.equal(body.request_id, "chatcmpl-xyz");
  assert.equal(body.duration_ms, 4321);
  assert.equal(body.error, "401 Unauthorized");
  assert.equal(body.error_status_code, 401);
  assert.equal(body.error_content_type, "application/json");
});

test("postTelemetry omits undefined QUB-105 fields (clean wire shape)", async () => {
  // Successful calls leave the error fields undefined;
  // JSON.stringify drops undefined so the wire payload stays
  // clean. The receiver treats the absent keys as nullable
  // columns.
  sent.length = 0;
  const telem = {
    model: "openrouter/x",
    provider: "openrouter",
    inputTokens: 1,
    outputTokens: 1,
    costUsd: 0.0001,
    stepCount: 1,
    requestId: "chatcmpl-1",
    durationMs: 100,
  };
  await postTelemetry(telem, makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 1);
  const body = JSON.parse(sent[0].init.body);
  assert.equal("error" in body, false, "no error on a successful call");
  assert.equal("error_status_code" in body, false);
  assert.equal("error_content_type" in body, false);
  assert.equal(body.request_id, "chatcmpl-1");
  assert.equal(body.duration_ms, 100);
});

test("postStatus retries once on 5xx, then gives up", async () => {
  sent.length = 0;
  const fr = makeFetchRetry([500, 500]);
  await postStatus("done", makeCtx(), { log: () => {}, fetchImpl: fr.fetch });
  assert.equal(fr.calls(), 2, "5xx should retry once and then give up");
});

test("postStatus does NOT retry on 4xx (auth/validation)", async () => {
  sent.length = 0;
  const fr = makeFetchRetry([401]);
  await postStatus("done", makeCtx(), { log: () => {}, fetchImpl: fr.fetch });
  assert.equal(fr.calls(), 1, "4xx is not retryable");
});

test("postStatus does NOT retry on 202 (run not yet persisted)", async () => {
  sent.length = 0;
  const fr = makeFetchRetry([202]);
  await postStatus("done", makeCtx(), { log: () => {}, fetchImpl: fr.fetch });
  assert.equal(fr.calls(), 1, "202 is not retryable — the runner will retry on the next stage");
});

test("postStatus surfaces 401 at error level so token-misconfig is visible (EH-007)", async () => {
  // A misconfigured BOOP_DASHBOARD_TOKEN silently drops
  // every telemetry row while the operator stares at an
  // empty dashboard. The 401 path now calls errlog()
  // instead of the generic "post rejected" log() so the
  // misconfig is visible in the runner's stderr.
  sent.length = 0;
  const errs = [];
  const fr = makeFetchRetry([401]);
  await postStatus(
    "done",
    makeCtx(),
    {
      log: () => {},
      errlog: (...args) => errs.push(args),
      fetchImpl: fr.fetch,
    },
  );
  assert.equal(fr.calls(), 1, "401 is not retryable");
  assert.equal(errs.length, 1, "401 emits exactly one errlog call");
  const [, msg] = errs[0];
  assert.ok(msg.includes("auth rejected"), "errlog msg = " + msg);
  assert.ok(msg.includes("BOOP_DASHBOARD_TOKEN"), "errlog msg points at the env var: " + msg);
});

test("postStatus attaches the reason to the payload as `error` (QUB-102)", async () => {
  // The QUB-102 dashboard plumbing forwards the abort or
  // gate-failure reason as the `error` field in the
  // statusRequest body. The receiver's RecordStatus stores
  // it via UpdateRunStatus and surfaces it on the dashboard
  // row so the operator can distinguish a QUB-102 abort
  // from a sniff-parse failure (both reach the receiver as
  // stage="failed"). Lockdown test: a future refactor that
  // drops the error field will fail here, not at the
  // orchestrator level.
  sent.length = 0;
  await postStatus(
    "failed",
    makeCtx(),
    { log: () => {}, fetchImpl: makeFetchOK() },
    "another pod already passed [fetch, handshake]; refusing to duplicate the review",
  );
  assert.equal(sent.length, 1);
  assert.equal(
    sent[0].init.body,
    JSON.stringify({
      stage: "failed",
      error: "another pod already passed [fetch, handshake]; refusing to duplicate the review",
    }),
  );
});

test("postTelemetry is a no-op when telemetry is null", async () => {
  sent.length = 0;
  await postTelemetry(null, makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 0);
});

test("postStage posts to /api/runs/{id}/stages with stage and optional meta (QUB-109)", async () => {
  sent.length = 0;
  await postStage("clone", makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() }, { meta: '{"path":"src/index.ts","line":42}' });
  assert.equal(sent.length, 1);
  assert.ok(sent[0].url.endsWith("/api/runs/boop-a-b-1-aaaaaaa/stages"), "URL path");
  assert.deepEqual(JSON.parse(sent[0].init.body), { stage: "clone", ended: false, meta: '{"path":"src/index.ts","line":42}' });
});

test("postStage flips `ended` to true on the end POST", async () => {
  sent.length = 0;
  await postStage("clone", makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() }, { ended: true });
  assert.deepEqual(JSON.parse(sent[0].init.body), { stage: "clone", ended: true });
});

test("postStage is a no-op when dashboardUrl is unset", async () => {
  sent.length = 0;
  await postStage("clone", { dashboardUrl: null, dashboardToken: "x", jobName: "j" }, { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 0);
});

test("postLensTelemetry sends a batch with one row per lens (QUB-109)", async () => {
  sent.length = 0;
  const lenses = [
    { lens: "security", model: "openai/gpt-4.1", costUsd: 0.05, inputTokens: 100, outputTokens: 50 },
    { lens: "deep", model: "anthropic/claude-3.7-sonnet", costUsd: 0.20, inputTokens: 400, outputTokens: 200, stepCount: 3 },
  ];
  await postLensTelemetry(lenses, makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 1);
  const body = JSON.parse(sent[0].init.body);
  assert.equal(body.lenses.length, 2);
  assert.equal(body.lenses[0].lens, "security");
  assert.equal(body.lenses[1].lens, "deep");
  assert.equal(body.lenses[1].cost_usd, 0.20);
  assert.equal(body.lenses[1].step_count, 3);
});

test("postLensTelemetry is a no-op on an empty array", async () => {
  sent.length = 0;
  await postLensTelemetry([], makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 0);
});

test("startHeartbeat returns a no-op stop when dashboardUrl is unset", () => {
  const stop = startHeartbeat({ dashboardUrl: null, dashboardToken: "x", jobName: "j" }, { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(typeof stop, "function");
  // Stop is idempotent.
  stop();
  stop();
});
