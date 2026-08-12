// Input validators and secret readers.
//
// All inputs that flow into `git` argv or a subprocess env must pass
// through `assertSafeRef` / `assertSafeSha` first. CVE-2017-1000117
// (and friends) lets a branch named `--upload-pack=evil` execute the
// attacker's command on the runner host; the regex below is the
// defense-in-depth gate even though the receiver validates too.
//
// `readSecretFile` reads a Secret-mounted file with mode 0400 and
// returns the trimmed contents. Errors are wrapped with a stable label
// so log triage can distinguish "mount missing" from "permission
// denied" from "empty file".

import type { Ctx, FsLike } from "../types.ts";

// safeRefCharsRegex lists the characters allowed in a refname.
// Structural rules (no leading `-` or `/`, no `..`, no trailing
// `.lock`) are enforced by assertSafeRef itself; the regex catches
// everything else.
export const safeRefCharsRegex = /^[A-Za-z0-9._/-]+$/;

// Hex SHA, 7 to 40 chars. Mirrors the receiver's validatePreviousHeadSHA
// so a SHA in the 8-39 char range (legal git short SHAs) does not pass
// the receiver and fail the runner.
export const safeShaRegex = /^[0-9a-f]{7,40}$/;

// assertSafeRef returns the ref unchanged if it matches the safe
// character set and is at least one character long, otherwise it
// throws. Use this on every PR-controlled string before it reaches
// `git` so a malicious refname becomes a 4xx-shaped runner failure
// instead of a CVE-shaped RCE.
export function assertSafeRef(name: string, value: unknown): string {
  if (typeof value !== "string" || !safeRefCharsRegex.test(value)) {
    throw new Error(`unsafe ${name}: ${JSON.stringify(value)}`);
  }
  // Leading `-` is a flag-injection vector: `git fetch origin
  // --upload-pack=evil` would execute the attacker's command. The
  // regex permits `-` anywhere except the very first character, so
  // we explicitly reject leading `-` here. `^` inside the regex
  // class excludes `-`.
  if (value.startsWith("-")) {
    throw new Error(`unsafe ${name} (leading '-'): ${JSON.stringify(value)}`);
  }
  // Git refnames cannot contain "..", cannot end with ".lock",
  // and cannot have a leading or trailing "/". These are also
  // shape-preserving for git CLI args, so we enforce them at the
  // gate.
  if (value.startsWith("/") || value.endsWith("/")) {
    throw new Error(`unsafe ${name} (leading/trailing '/'): ${JSON.stringify(value)}`);
  }
  if (value.includes("..")) {
    throw new Error(`unsafe ${name} ('..' segment): ${JSON.stringify(value)}`);
  }
  if (value.endsWith(".lock")) {
    throw new Error(`unsafe ${name} ('.lock' suffix): ${JSON.stringify(value)}`);
  }
  return value;
}

// assertSafeSha is the SHA-shaped variant. Used for previousHeadSHA
// before passing to `git fetch origin <sha>`.
export function assertSafeSha(name: string, value: unknown): string {
  if (typeof value !== "string" || !safeShaRegex.test(value)) {
    throw new Error(`unsafe ${name}: ${JSON.stringify(value)}`);
  }
  return value;
}

// reviewRange resolves the git range every diff consumer walks. On a
// re-review (reviewNumber > 1 with a previousHeadSha) the model sees
// only the delta since the last review, so the range is
// `previousHeadSha...prHeadSha`; on a first review it is
// `prBaseRef...prHeadSha`. Every component passes through the public
// asserts — this is the single gate that keeps PR-controlled refs out
// of `git diff` argv, shared by the prompt text and the git_diff tool.
export function reviewRange(ctx: Partial<Ctx> | undefined | null): string | undefined {
  const { reviewNumber, previousHeadSha, prBaseRef, prHeadSha } = ctx ?? {};
  if (!prBaseRef || !prHeadSha) {
    return undefined;
  }
  const baseRef = assertSafeRef("PR_BASE_REF", prBaseRef);
  assertSafeSha("PR_HEAD_SHA", prHeadSha);
  if (typeof reviewNumber === "number" && reviewNumber > 1 && previousHeadSha) {
    assertSafeSha("PR_PREVIOUS_HEAD_SHA", previousHeadSha);
    return `${previousHeadSha}...${prHeadSha}`;
  }
  return `${baseRef}...${prHeadSha}`;
}

export function shortSha(s: string | null | undefined): string {
  return s && s.length >= 7 ? s.slice(0, 7) : (s || "");
}

// readSecretFile reads a Secret-mounted file with mode 0400 and
// returns the trimmed contents. Errors are wrapped with a stable
// label so log triage can distinguish "mount missing" from
// "permission denied" from "empty file".
export async function readSecretFile(
  label: string,
  path: string,
  fs: FsLike,
): Promise<string> {
  if (!path) {
    throw new Error(`missing secret mount path for ${label}`);
  }
  let raw: string | Buffer;
  try {
    raw = await fs.readFile(path, "utf8");
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    throw new Error(`read ${label} at ${path}: ${msg}`);
  }
  // Secrets must be non-empty. A zero-byte file usually means the
  // mount is broken (SubPath typo, missing Secret, wrong key name)
  // — better to fail loud than to authenticate with an empty key.
  const trimmed = String(raw).replace(/\s+$/u, "");
  if (trimmed.length === 0) {
    throw new Error(`empty ${label} at ${path}`);
  }
  return trimmed;
}
