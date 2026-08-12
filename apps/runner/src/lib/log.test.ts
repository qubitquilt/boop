import { test } from "node:test";
import assert from "node:assert/strict";

import { makeLogger } from "./log.ts";

const ctx = {
  prOwner: "qubitquilt",
  prRepo: "boop",
  prNumber: "42",
  prHeadSha: "87bcc09abcdef0123456789abcdef0123456789",
};

test("makeLogger.log emits INFO line with pr/sha stamped", () => {
  const writes = [];
  const realWrite = process.stdout.write.bind(process.stdout);
  process.stdout.write = (chunk) => {
    writes.push(chunk);
    return true;
  };
  try {
    const { log } = makeLogger(ctx);
    log("clone", "starting", { foo: "bar" });
  } finally {
    process.stdout.write = realWrite;
  }
  assert.equal(writes.length, 1);
  const line = writes[0].trim();
  const obj = JSON.parse(line);
  assert.equal(obj.level, "INFO");
  assert.equal(obj.stage, "clone");
  assert.equal(obj.msg, "starting");
  assert.equal(obj.pr, "qubitquilt/boop#42");
  assert.equal(obj.sha, "87bcc09abcdef0123456789abcdef0123456789");
  assert.equal(obj.foo, "bar");
});

test("makeLogger.errlog emits ERROR line", () => {
  const writes = [];
  const realWrite = process.stdout.write.bind(process.stdout);
  process.stdout.write = (chunk) => {
    writes.push(chunk);
    return true;
  };
  try {
    const { errlog } = makeLogger(ctx);
    errlog("cleanup", "boom", { err: "ENOENT" });
  } finally {
    process.stdout.write = realWrite;
  }
  const obj = JSON.parse(writes[0].trim());
  assert.equal(obj.level, "ERROR");
  assert.equal(obj.stage, "cleanup");
  assert.equal(obj.msg, "boom");
  assert.equal(obj.err, "ENOENT");
});

test("makeLogger works with partial ctx (no prHeadSha)", () => {
  const writes = [];
  const realWrite = process.stdout.write.bind(process.stdout);
  process.stdout.write = (chunk) => {
    writes.push(chunk);
    return true;
  };
  try {
    const { log } = makeLogger({ prOwner: "o", prRepo: "r", prNumber: "1", prHeadSha: "" });
    log("start", "ok");
  } finally {
    process.stdout.write = realWrite;
  }
  const obj = JSON.parse(writes[0].trim());
  assert.equal(obj.pr, "o/r#1");
  assert.equal(obj.sha, "");
});
