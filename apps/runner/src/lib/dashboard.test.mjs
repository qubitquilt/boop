import { test } from "node:test";
import assert from "node:assert/strict";

import { postStatus, postTelemetry } from "./dashboard.mjs";

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

test("postTelemetry is a no-op when telemetry is null", async () => {
  sent.length = 0;
  await postTelemetry(null, makeCtx(), { log: () => {}, fetchImpl: makeFetchOK() });
  assert.equal(sent.length, 0);
});
