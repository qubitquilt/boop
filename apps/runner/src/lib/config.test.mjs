import { test } from "node:test";
import assert from "node:assert/strict";

import { loadConfig } from "./config.mjs";

const validEnv = {
  PR_OWNER: "qubitquilt",
  PR_REPO: "boop",
  PR_NUMBER: "42",
  PR_HEAD_SHA: "87bcc09abcdef0123456789abcdef0123456789",
  PR_BASE_REF: "main",
  GITHUB_APP_ID: "12345",
  GITHUB_APP_INSTALLATION_ID: "67890",
};

test("loadConfig parses required vars", () => {
  const ctx = loadConfig(validEnv);
  assert.equal(ctx.prOwner, "qubitquilt");
  assert.equal(ctx.prRepo, "boop");
  assert.equal(ctx.prNumber, "42");
  assert.equal(ctx.prHeadSha, "87bcc09abcdef0123456789abcdef0123456789");
  assert.equal(ctx.prBaseRef, "main");
  assert.equal(ctx.githubAppId, "12345");
  assert.equal(ctx.githubAppInstallationId, "67890");
});

test("loadConfig parses optional vars", () => {
  const ctx = loadConfig({
    ...validEnv,
    PR_PREVIOUS_HEAD_SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    BOOP_STATUS_COMMENT_ID: "111",
    BOOP_REACTION_COMMENT_ID: "222",
    BOOP_REVIEW_NUMBER: "3",
    BOOP_BOT_LOGIN: "booppr[bot]",
    BOOP_SKIP_SKILL: "1",
    BOOP_DEBUG: "1",
  });
  assert.equal(ctx.previousHeadSha, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa");
  assert.equal(ctx.statusCommentId, 111);
  assert.equal(ctx.reactionCommentId, 222);
  assert.equal(ctx.reviewNumber, 3);
  assert.equal(ctx.botLogin, "booppr[bot]");
  assert.equal(ctx.skipSkill, true);
  assert.equal(ctx.debug, true);
});

test("loadConfig fills sensible defaults", () => {
  const ctx = loadConfig(validEnv);
  assert.equal(ctx.reviewNumber, 1);
  assert.equal(ctx.previousHeadSha, null);
  assert.equal(ctx.statusCommentId, null);
  assert.equal(ctx.reactionCommentId, null);
  assert.equal(ctx.botLogin, null);
  assert.equal(ctx.skipSkill, false);
  assert.equal(ctx.debug, false);
});

test("loadConfig falls back to default secret paths", () => {
  const ctx = loadConfig(validEnv);
  assert.equal(ctx.privateKeyPath, "/secrets/github-app-private-key");
  assert.equal(ctx.openrouterKeyPath, "/secrets/openrouter-api-key");
});

test("loadConfig honours explicit secret paths", () => {
  const ctx = loadConfig({
    ...validEnv,
    BOOP_GITHUB_APP_PRIVATE_KEY_PATH: "/custom/key",
    BOOP_OPENROUTER_API_KEY_PATH: "/custom/key2",
  });
  assert.equal(ctx.privateKeyPath, "/custom/key");
  assert.equal(ctx.openrouterKeyPath, "/custom/key2");
});

test("loadConfig rejects missing PR_OWNER", () => {
  const env = { ...validEnv };
  delete env.PR_OWNER;
  assert.throws(() => loadConfig(env), /PR_OWNER/);
});

test("loadConfig rejects missing PR_REPO", () => {
  const env = { ...validEnv };
  delete env.PR_REPO;
  assert.throws(() => loadConfig(env), /PR_REPO/);
});

test("loadConfig rejects missing PR_NUMBER", () => {
  const env = { ...validEnv };
  delete env.PR_NUMBER;
  assert.throws(() => loadConfig(env), /PR_NUMBER/);
});

test("loadConfig rejects missing PR_HEAD_SHA", () => {
  const env = { ...validEnv };
  delete env.PR_HEAD_SHA;
  assert.throws(() => loadConfig(env), /PR_HEAD_SHA/);
});

test("loadConfig rejects missing PR_BASE_REF", () => {
  const env = { ...validEnv };
  delete env.PR_BASE_REF;
  assert.throws(() => loadConfig(env), /PR_BASE_REF/);
});

test("loadConfig rejects missing GITHUB_APP_ID", () => {
  const env = { ...validEnv };
  delete env.GITHUB_APP_ID;
  assert.throws(() => loadConfig(env), /GITHUB_APP_ID/);
});

test("loadConfig rejects missing GITHUB_APP_INSTALLATION_ID", () => {
  const env = { ...validEnv };
  delete env.GITHUB_APP_INSTALLATION_ID;
  assert.throws(() => loadConfig(env), /GITHUB_APP_INSTALLATION_ID/);
});

test("loadConfig rejects non-positive integer for status comment", () => {
  const env = { ...validEnv, BOOP_STATUS_COMMENT_ID: "0" };
  const ctx = loadConfig(env);
  assert.equal(ctx.statusCommentId, null);
});

test("loadConfig rejects non-numeric status comment", () => {
  const env = { ...validEnv, BOOP_STATUS_COMMENT_ID: "abc" };
  const ctx = loadConfig(env);
  assert.equal(ctx.statusCommentId, null);
});

test("loadConfig defaults reviewNumber to 1 when unset", () => {
  const ctx = loadConfig(validEnv);
  assert.equal(ctx.reviewNumber, 1);
});

test("loadConfig defaults reviewNumber to 1 when invalid", () => {
  const ctx = loadConfig({ ...validEnv, BOOP_REVIEW_NUMBER: "foo" });
  assert.equal(ctx.reviewNumber, 1);
});
