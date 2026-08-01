import { test } from "node:test";
import assert from "node:assert/strict";

import {
  safeRefCharsRegex,
  safeShaRegex,
  assertSafeRef,
  assertSafeSha,
  shortSha,
  readSecretFile,
} from "./security.mjs";

// H1: every refname the runner passes to `git` must be validated
// against a strict regex. A branch named `--upload-pack=evil` would
// otherwise let git execute the attacker's command (CVE-2017-1000117).
// These cases pin the contract: empty, control chars, leading
// dashes, double-dots, slashes at the boundary, lock suffixes —
// all rejected; ordinary refs accepted unchanged.
test("safeRefCharsRegex and safeShaRegex expose the exact patterns", () => {
  assert.equal(safeRefCharsRegex.source, "^[A-Za-z0-9._/-]+$");
  assert.equal(safeShaRegex.source, "^[0-9a-f]{7,40}$");
});

test("assertSafeRef accepts ordinary refs", () => {
  for (const ref of ["main", "develop", "feature/foo-bar", "v1.2.3", "user/alice/branch_42"]) {
    assert.equal(assertSafeRef("ref", ref), ref);
  }
});

test("assertSafeRef rejects CVE-2017-1000117 shapes", () => {
  for (const ref of [
    "",
    "--upload-pack=evil",
    "-x",
    "-",
    "--",
    "../etc/passwd",
    "main --upload-pack=evil",
    "main\n",
    "main\revil",
    "main;rm -rf /",
    "main|cat /etc/passwd",
    "main$IFS",
    "main`evil`",
    "/main",
    "main/",
    "main..branch",
    "main.lock",
    "main\u0000branch",
    "main branch",
    "main*",
  ]) {
    assert.throws(
      () => assertSafeRef("ref", ref),
      /unsafe ref/,
      `expected reject: ${JSON.stringify(ref)}`,
    );
  }
});

test("assertSafeRef returns the input when safe (identity round-trip)", () => {
  assert.equal(assertSafeRef("base", "main"), "main");
  assert.equal(assertSafeRef("base", "feature/x.y_z-1"), "feature/x.y_z-1");
});

test("assertSafeSha accepts git SHAs only", () => {
  for (const sha of [
    "abc1234",
    "87bcc09abcdef0123456789abcdef0123456789a",
    "0123456789abcdef0123456789abcdef01234567",
  ]) {
    assert.equal(assertSafeSha("sha", sha), sha);
  }
});

test("assertSafeSha rejects non-SHA input", () => {
  for (const sha of [
    "",
    "not-a-sha",
    "ZZZ1234",
    "20cd521abcdef0123456789abcdef012345678900",
    "../etc/passwd",
    "main",
  ]) {
    assert.throws(
      () => assertSafeSha("sha", sha),
      /unsafe sha/,
      `expected reject: ${JSON.stringify(sha)}`,
    );
  }
});

test("shortSha preserves short and trims long", () => {
  assert.equal(shortSha(""), "");
  assert.equal(shortSha(null), "");
  assert.equal(shortSha("abc"), "abc");
  assert.equal(shortSha("abc1234"), "abc1234");
  assert.equal(shortSha("abc1234567890"), "abc1234");
});

// --- readSecretFile -----------------------------------------------------

test("readSecretFile trims trailing whitespace", async () => {
  const fakeFs = { readFile: async () => "  abc\n\n" };
  const v = await readSecretFile("X", "/p", fakeFs);
  // Leading whitespace is preserved (the mount is mode 0400; we
  // never want to silently swallow a leading whitespace difference
  // that might be intentional). Trailing whitespace is stripped.
  assert.equal(v, "  abc");
});

test("readSecretFile rejects whitespace-only file", async () => {
  const fakeFs = { readFile: async () => "\n\n" };
  await assert.rejects(
    () => readSecretFile("X", "/p", fakeFs),
    /empty X at \/p/,
  );
});

test("readSecretFile rejects missing path", async () => {
  await assert.rejects(
    () => readSecretFile("X", "", {}),
    /missing secret mount path/,
  );
});

test("readSecretFile wraps ENOENT with stable label", async () => {
  const fakeFs = { readFile: async () => { throw new Error("ENOENT"); } };
  await assert.rejects(
    () => readSecretFile("X", "/secrets/x", fakeFs),
    /read X at \/secrets\/x: ENOENT/,
  );
});

test("readSecretFile passes through non-Error throws", async () => {
  const fakeFs = { readFile: async () => { throw "string-error"; } };
  await assert.rejects(
    () => readSecretFile("X", "/secrets/x", fakeFs),
    /string-error/,
  );
});
