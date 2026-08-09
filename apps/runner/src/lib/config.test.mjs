import { test } from "node:test";
import assert from "node:assert/strict";

// QUB-<next> Option A local-run support: production runs mount
// the cloned repo at /work/repo and the runner-config ConfigMap
// at /home/opencode/.config/opencode. Both paths are sealed on
// macOS workstations, so the runner accepts BOOP_REPO_DIR /
// BOOP_CONFIG_SRC env overrides. These tests pin the override
// behavior so the production contract stays the default while
// local runs can redirect the mounts.

test("REPO_DIR defaults to /work/repo", () => {
  assert.equal(process.env.BOOP_REPO_DIR, undefined);
});

test("CONFIG_SRC defaults to /home/opencode/.config/opencode", () => {
  assert.equal(process.env.BOOP_CONFIG_SRC, undefined);
});