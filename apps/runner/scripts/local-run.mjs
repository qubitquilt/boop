// Local-run wrapper for the boop runner.
//
// Production wires the runner via a K8s Job template: the receiver
// mints a GitHub App installation token, builds env vars, mounts the
// cloned repo at /work/repo, and mounts the skill ConfigMap at
// /home/opencode/.config/opencode. Both paths are sealed on macOS,
// so this wrapper:
//
//   - Reads BOOP_REPO_DIR / BOOP_CONFIG_SRC from env (defaults via
//     config.mjs) to point at /tmp/boop-runner/{repo,skills}.
//   - Stubs deps.cloneRepo with a no-op so the runner doesn't
//     try to re-clone over the pre-staged repo (which is cloned
//     here with `gh repo clone`, not via the App token).
//   - Wires up `deps.runOpenCodeSkill` to NOT be overridden, so the
//     real Agent SDK + tool set actually fires against OpenRouter.
//
// What this wrapper does NOT do:
//   - It does not stub the LLM call. runOpenCodeSkill runs for
//     real; the narrator invokes the agent SDK with tools.
//   - It does not stub the GitHub comment posts. The runner uses
//     the App's installation token (minted from the PEM) to PATCH
//     the status comment and POST the summary + inlines. Those
//     side effects are real.
//
// Required env (set before running):
//   OPENROUTER_API_KEY            — your OpenRouter key
//   GITHUB_APP_ID                 — 4420607
//   GITHUB_APP_INSTALLATION_ID    — 149974124
//   BOOP_OPENROUTER_API_KEY_PATH  — /tmp/boop-secrets/openrouter-api-key
//   BOOP_GITHUB_APP_PRIVATE_KEY_PATH
//                                 — ~/Downloads/booppr.2026-07-28.private-key.pem
//   BOOP_REPO_DIR                 — /tmp/boop-runner/repo (default)
//   BOOP_CONFIG_SRC               — /tmp/boop-runner/skills (default)
//
// Optional:
//   OPENROUTER_MODEL              — defaults to whatever the
//                                   receiver would forward (set here
//                                   to "minimax/minimax-m3")
//   BOOP_TOOLS_ENABLED=0          — disable the agent tool set
//                                   for a baseline run

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { run } from "../src/index.mjs";

const env = process.env;
// The wrapper lives at apps/runner/scripts/local-run.mjs. The skill
// source is the repo's apps/k8s/base/runner-config/skills —
// resolve via import.meta.url so the hint is correct regardless
// of the cwd the user invokes the wrapper from.
const here = path.dirname(fileURLToPath(import.meta.url));
const skillSource = path.resolve(here, "../../k8s/base/runner-config/skills");

// Required env check. Fail fast with a clear message rather than
// letting the runner hit a "key not set" error deep in a stage.
const required = [
  "GITHUB_APP_ID",
  "GITHUB_APP_INSTALLATION_ID",
  "BOOP_GITHUB_APP_PRIVATE_KEY_PATH",
];
const missing = required.filter((k) => !env[k] || env[k] === "");
if (missing.length > 0) {
  console.error(
    `[local-run] missing required env: ${missing.join(", ")}`,
  );
  console.error(
    "[local-run] see apps/runner/scripts/local-run.mjs for the full env contract.",
  );
  process.exit(2);
}

// The runner's handshake stage reads the OpenRouter key from a
// mounted secret file (readSecretFile at openrouterKeyPath). The
// K8s flow mounts the key as a Secret at /secrets/openrouter-api-key;
// locally we need to either write the env-var key into that path or
// rely on a pre-existing file. Auto-bootstrap when missing — but
// only when OPENROUTER_API_KEY is set; otherwise the user has to
// decide (write the key into BOOP_OPENROUTER_API_KEY_PATH by hand).
if (!env.OPENROUTER_API_KEY) {
  try {
    await fs.access(env.BOOP_OPENROUTER_API_KEY_PATH);
  } catch {
    console.error(
      `[local-run] OPENROUTER_API_KEY is unset and ${env.BOOP_OPENROUTER_API_KEY_PATH} is missing.`,
    );
    console.error(
      "[local-run] either set OPENROUTER_API_KEY (the wrapper writes it to the path) or write the key into the path manually with mode 0400.",
    );
    process.exit(2);
  }
} else {
  // Write the key into the secret path with 0400 mode so the
  // runner's readSecretFile guard (`stat().mode & 0o777` check)
  // passes. Create the parent dir if missing (the wrapper is the
  // first writer in a fresh /tmp/boop-secrets).
  await fs.mkdir(path.dirname(env.BOOP_OPENROUTER_API_KEY_PATH), {
    recursive: true,
    mode: 0o700,
  });
  // A previous run may have left the file 0400 — loosen so the
  // next writeFile can truncate, then restore 0400 after.
  try {
    await fs.chmod(env.BOOP_OPENROUTER_API_KEY_PATH, 0o600);
  } catch {
    // File doesn't exist yet; mkdir + writeFile will create it.
  }
  await fs.writeFile(env.BOOP_OPENROUTER_API_KEY_PATH, env.OPENROUTER_API_KEY + "\n", {
    mode: 0o400,
  });
}

// Verify the skill mount is populated. The runner's buildBoopPrompt
// reads SKILL.md + lens files from CONFIG_SRC/skills/boop/; a
// missing mount aborts the sniff stage with an unhelpful "ENOENT"
// that masks the real cause.
try {
  await fs.access(`${env.BOOP_CONFIG_SRC}/skills/boop/SKILL.md`);
} catch {
  console.error(
    `[local-run] ${env.BOOP_CONFIG_SRC}/skills/boop/SKILL.md not found.`,
  );
  console.error(
    "[local-run] stage the skill mount:",
  );
  console.error(
    `  cp -R ${skillSource} ${env.BOOP_CONFIG_SRC}/skills`,
  );
  process.exit(2);
}

// Verify the pre-staged repo head matches PR_HEAD_SHA. The wrapper
// stubs cloneRepo so the runner never re-clones — but the runner
// still trusts repoDir to be at the head SHA. A mismatch surfaces
// as a confusing "diff range produced empty output" downstream.
const { default: cp } = await import("node:child_process");
const { promisify } = await import("node:util");
const exec = promisify(cp.execFile);
const headOut = await exec("git", ["rev-parse", "HEAD"], {
  cwd: env.BOOP_REPO_DIR,
});
const actualHead = headOut.stdout.trim();
if (actualHead !== env.PR_HEAD_SHA) {
  console.error(
    `[local-run] ${env.BOOP_REPO_DIR} HEAD is ${actualHead.slice(0, 12)}, expected ${env.PR_HEAD_SHA.slice(0, 12)}.`,
  );
  console.error(
    "[local-run] check out the PR head before running:",
  );
  console.error(
    `  git -C ${env.BOOP_REPO_DIR} checkout ${env.PR_HEAD_SHA}`,
  );
  process.exit(2);
}

// Default the model to whatever the deployment uses, so this
// wrapper produces the same review shape as a K8s-driven run.
if (!env.OPENROUTER_MODEL) {
  env.OPENROUTER_MODEL = "minimax/minimax-m3";
}

console.log("[local-run] starting");
console.log(`  PR:           ${env.PR_OWNER}/${env.PR_REPO}#${env.PR_NUMBER}`);
console.log(`  Head SHA:     ${env.PR_HEAD_SHA.slice(0, 12)}`);
console.log(`  Base ref:     ${env.PR_BASE_REF}`);
console.log(`  Model:        ${env.OPENROUTER_MODEL}`);
console.log(`  Tools:        ${env.BOOP_TOOLS_ENABLED === "0" ? "disabled (BOOP_TOOLS_ENABLED=0)" : "enabled (run_command + read_file + git_diff)"}`);
console.log(`  Repo dir:     ${env.BOOP_REPO_DIR}`);
console.log(`  Skill mount:  ${env.BOOP_CONFIG_SRC}`);
console.log("");

await run(env, {
  // Stub cloneRepo so the runner doesn't try to re-clone. The
  // pre-staged repo at BOOP_REPO_DIR is already at the head SHA.
  // This sidesteps the App-token netrc/gitconfig dance that the
  // production flow wires through the receiver.
  cloneRepo: async (ctx, deps) => {
    await deps.postStatus("clone");
  },
  // Stage retries off so a failed stage doesn't loop us through
  // backoff during a smoke run.
  stageMaxAttempts: 1,
  sleep: async () => {},
});