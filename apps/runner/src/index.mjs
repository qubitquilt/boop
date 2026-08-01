// Boop runner.
//
// Orchestrates one PR review:
//   1. Reads the GitHub App private key and OpenRouter API key from
//      their mounted Secret files (not env — env is visible to a
//      prompt-injected LLM via /proc/self/environ).
//   2. Mints a GitHub App installation token.
//   3. Posts a status comment on the PR (the receiver pre-creates
//      one; if BOOP_STATUS_COMMENT_ID is set we PATCH it, otherwise
//      we post a fresh one as a fallback).
//   4. Clones the PR at the head SHA into /work/repo using a
//      short-lived git credential helper (so the token never
//      appears in argv, .git/config, or process listings).
//   5. Validates PR_BASE_REF and previousHeadSHA against a strict
//      regex before passing them to `git fetch` / `git checkout` to
//      defeat CVE-2017-1000117-class argument injection (a branch
//      named `--upload-pack=evil` would otherwise execute the
//      attacker's command on the runner host).
//   6. Runs `opencode run` against /work/repo with the boop skill
//      prompt. Hard-kills the subprocess after 25 min so a hung
//      call cannot pin the Job past its 30-min deadline.
//   7. PATCHes the status comment at each stage with the latest
//      emoji + message.
//   8. Posts the review body to the PR as a single comment.
//
// Structure: this file is the orchestrator. The work is in
// `./lib/*.mjs` (config, log, security, git, opencode, github).
// Each lib module accepts a `ctx` (loaded config) and a `deps`
// bundle so the whole pipeline is unit-testable without env vars,
// real Octokit, real network, or real `git`.

import fs from "node:fs/promises";
import jwt from "jsonwebtoken";
import { spawn } from "node:child_process";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import {
  loadConfig,
  REPO_DIR,
  CONFIG_SRC,
  WRITABLE_HOME,
  WRITABLE_CONFIG,
  CONFIG_DIR,
  NETRC_PATH,
  GITCONFIG_PATH,
} from "./lib/config.mjs";
import { makeLogger } from "./lib/log.mjs";
import { assertSafeRef, assertSafeSha, readSecretFile } from "./lib/security.mjs";
import { createCleanupRegistry, cloneRepo } from "./lib/git.mjs";
import { runOpenCodeSkill } from "./lib/opencode.mjs";
import {
  mintInstallationToken,
  makeOctokit,
  postReview,
  postInlineComments,
  postStatus,
  cleanupPriorReview,
} from "./lib/github.mjs";

const execFileAsync = promisify(execFile);

// makeDeps bundles every side-effecting dependency the pipeline
// passes around. Each lib function reads only what it needs from
// `deps`; tests can swap any field (spawnFn, fetchImpl, OctokitCtor)
// to drive a deterministic scenario.
function makeDeps(ctx, log, cleanup) {
  return {
    fs,
    execFile: execFileAsync,
    spawnFn: spawn,
    jwt,
    fetchImpl: fetch,
    OctokitCtor: undefined, // lib/github.mjs falls back to the real Octokit
    paths: {
      repoDir: REPO_DIR,
      configSrc: CONFIG_SRC,
      configDir: CONFIG_DIR,
      writableHome: WRITABLE_HOME,
      writableConfig: WRITABLE_CONFIG,
      netrc: NETRC_PATH,
      gitconfig: GITCONFIG_PATH,
    },
    cleanup,
    log: log.log,
    errlog: log.errlog,
    postStatus: (stage, detail) => postStatus(stage, detail, ctx, octokitDeps(log)),
  };
}

// octokitDeps is the subset of deps that postStatus needs. Kept as a
// helper so the postStatus reference in makeDeps always reflects the
// current octokit (it's set after the installation token is minted).
function octokitDeps(log) {
  return { log: log.log, errlog: log.errlog, octokit: currentOctokit };
}

let currentOctokit = null;

// run is the pipeline. Exported so tests can drive it with a fixture
// ctx + injected deps; index.mjs is just the entry invocation.
//
// `overrides` lets a test swap any side-effecting dep (spawn, exec,
// fetch, jwt, fs) without monkey-patching the module — every lib
// function reads its dependencies off the `deps` bundle, so the
// override simply replaces the corresponding field. `runOpenCodeSkill`
// and `cleanupPriorReview` are also overridable so a test can return
// canned output without actually running opencode or hitting GitHub.
export async function run(env = process.env, overrides = {}) {
  const ctx = loadConfig(env);
  const log = makeLogger(ctx);
  const cleanup = createCleanupRegistry({ errlog: log.errlog });

  // The Octokit instance has its own copy of the token inside, but
  // the local var lives in this scope. There is no secure way to
  // wipe a JS string in-place (V8 interns and may share the backing
  // buffer), so we instead limit the token's lifetime to the rest
  // of this function and rely on the netrc + gitconfig cleanup
  // hooks (see writeNetrc / writeGitconfig in lib/git.mjs) to make
  // any persistent copy unreachable.
  const deps = {
    ...makeDeps(ctx, log, cleanup),
    ...overrides, // overrides.spawnFn, .execFile, .fetchImpl, .jwt, .fs
  };

  // Validate every PR-controlled refname BEFORE it touches `git` or
  // any subprocess argv. validateBaseRef in the receiver is the
  // first gate; this is the second (defense-in-depth: a future
  // change in the receiver shouldn't be load-bearing here).
  assertSafeRef("PR_BASE_REF", ctx.prBaseRef);
  if (ctx.previousHeadSha) {
    assertSafeSha("PR_PREVIOUS_HEAD_SHA", ctx.previousHeadSha);
  }

  log.log("start", "boop runner starting", {
    status_comment_id: ctx.statusCommentId,
    reaction_comment_id: ctx.reactionCommentId,
    review_number: ctx.reviewNumber,
    previous_head_sha: ctx.previousHeadSha,
    bot_login: ctx.botLogin,
  });

  // Read secrets from mounted files. Each value is stored in a
  // local const so it doesn't leak into process.env (it never
  // leaves this function — see runOpencode in lib/opencode.mjs for
  // the env stripping).
  const GITHUB_APP_PRIVATE_KEY = await readSecretFile(
    "GITHUB_APP_PRIVATE_KEY",
    ctx.privateKeyPath,
    deps.fs,
  );
  const OPENROUTER_API_KEY = await readSecretFile(
    "OPENROUTER_API_KEY",
    ctx.openrouterKeyPath,
    deps.fs,
  );

  // Mint installation token first; we need it for the status API too.
  const installationToken = await mintInstallationToken(
    ctx.githubAppId,
    GITHUB_APP_PRIVATE_KEY,
    ctx.githubAppInstallationId,
    deps,
  );
  currentOctokit = overrides.makeOctokit
    ? overrides.makeOctokit(installationToken)
    : makeOctokit(installationToken);
  log.log("auth", "minted installation token");
  await deps.postStatus("auth");

  try {
    try {
      const skillFn = overrides.runOpenCodeSkill || runOpenCodeSkill;
      const review = await skillFn(OPENROUTER_API_KEY, ctx, deps);
      log.log("review", "opencode returned", {
        summaryBytes: review.summary.length,
        inlineCount: review.inlineComments.length,
        confidence: review.confidence,
      });

      await postReview(currentOctokit, review.summary, ctx.reviewNumber, review.confidence, ctx);
      log.log("done", "summary comment posted", {
        review_number: ctx.reviewNumber,
        confidence: review.confidence,
      });

      await postInlineComments(currentOctokit, review.inlineComments, ctx, {
        log: log.log,
        errlog: log.errlog,
      });

      // On re-reviews, retire prior Boop artifacts so the PR thread
      // looks pristine. Best-effort: any error is logged but the
      // review still completes. Skipped on the first review (nothing
      // to clean) and when ctx.botLogin is unset (the receiver didn't
      // know the bot login — most commonly because the App is
      // configured to omit it).
      if (ctx.reviewNumber > 1 && ctx.botLogin) {
        try {
          const cleanupFn = overrides.cleanupPriorReview || cleanupPriorReview;
          const cleaned = await cleanupFn(installationToken, ctx, deps);
          if (cleaned.resolved > 0 || cleaned.minimized > 0) {
            log.log("cleanup", "retired prior review artifacts", cleaned);
          } else {
            log.log("cleanup", "no prior artifacts to retire");
          }
        } catch (err) {
          log.errlog("cleanup", "prior-review cleanup failed", {
            err: String(err?.message ?? err),
          });
        }
      }

      await deps.postStatus("done");
    } catch (err) {
      log.errlog("review", "opencode failed", { error: String(err?.message ?? err) });
      await deps.postStatus("failed", String(err?.message ?? err));
      throw err;
    }
  } finally {
    // Always scrub credentials and tmp artefacts, even on failure.
    // Order matters: revoke the netrc / gitconfig before unlinking
    // them so a future `git fetch` against the in-memory state
    // cannot read the token.
    await cleanup.runAll();
  }
}

// Only invoke run() when this module is the process entry point.
// Tests import individual helpers via `import { run } from
// "./index.mjs"` and must not trigger the review pipeline (which
// would try to call GitHub with an empty token).
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  run().catch(async (err) => {
    const ctx = (() => {
      try { return loadConfig(process.env); } catch { return {}; }
    })();
    const log = makeLogger({
      prOwner: ctx.prOwner || "?",
      prRepo: ctx.prRepo || "?",
      prNumber: ctx.prNumber || "?",
      prHeadSha: ctx.prHeadSha || "?",
    });
    log.errlog("fatal", "boop runner failed", { error: String(err?.message ?? err) });
    if (currentOctokit && ctx.statusCommentId) {
      await postStatus("failed", String(err?.message ?? err), ctx, {
        log: log.log,
        errlog: log.errlog,
        octokit: currentOctokit,
      });
    }
    process.exit(1);
  });
}
