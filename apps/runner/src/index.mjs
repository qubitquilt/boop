// Boop runner.
//
// Orchestrates one PR review:
//   1. Mints a GitHub App installation token.
//   2. Posts a status comment on the PR (the receiver pre-creates
//      one; if BOOP_STATUS_COMMENT_ID is set we PATCH it, otherwise
//      we post a fresh one as a fallback).
//   3. Clones the PR at the head SHA into /work/repo.
//   4. Runs `opencode run` against /work/repo with the boop skill
//      prompt. Hard-kills the subprocess after 25 min so a hung
//      call cannot pin the Job past its 30-min deadline.
//   5. PATCHes the status comment at each stage with the latest
//      emoji + message.
//   6. Posts the review body to the PR as a single comment.

import { Octokit } from "@octokit/rest";
import jwt from "jsonwebtoken";
import { spawn } from "node:child_process";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import { reviewHeader } from "./review-header.mjs";

const execFileAsync = promisify(execFile);

const {
  GITHUB_APP_ID,
  GITHUB_APP_INSTALLATION_ID,
  GITHUB_APP_PRIVATE_KEY,
  PR_OWNER,
  PR_REPO,
  PR_NUMBER,
  PR_HEAD_SHA,
  PR_BASE_REF,
  PR_PREVIOUS_HEAD_SHA,
  OPENROUTER_API_KEY,
  BOOP_STATUS_COMMENT_ID,
  BOOP_REACTION_COMMENT_ID,
  BOOP_REVIEW_NUMBER,
  BOOP_BOT_LOGIN,
} = process.env;

const REPO_DIR = "/work/repo";
const CONFIG_SRC = "/home/opencode/.config/opencode";
const WRITABLE_HOME = "/tmp/opencode-home";
const WRITABLE_CONFIG = "/tmp/opencode-config";
const CONFIG_DIR = `${WRITABLE_CONFIG}/opencode`;

// Hard ceiling on the opencode subprocess. The Job has a 30-min
// activeDeadlineSeconds; keep some headroom for the post-review work.
const OPENCODE_TIMEOUT_MS = 25 * 60 * 1000;

// Status stages. The receiver uses the same vocabulary so the user
// can correlate GitHub comment updates with runner log lines.
const STATUS = {
  initial: "🐾 **Boop is on the case!** Sniffing through this PR at `{sha}`. Updates will appear here.",
  auth: "🔐 **Boop has arrived** — authenticated with GitHub at `{sha}`.",
  clone: "📥 **Boop fetched the repo** at `{sha}`. Checking out the PR head and starting the multi-lens review.",
  review: "🧠 **Boop is reviewing** — running the multi-lens review on `{sha}`.",
  done: "✅ **Boop's review is in.** See the comment below.",
  failed: "❌ **Boop got distracted.** Check the Job logs for details.",
};


// Short labels used in the timeline. The header above always
// shows the full state; the timeline is a one-line-per-stage log.
const SHORT = {
  auth: "🔐 authenticated",
  clone: "📥 fetched",
  review: "🧠 reviewing",
  done: "✅ review in",
  failed: "❌ distracted",
};
function shortSha(s) {
  return s && s.length >= 7 ? s.slice(0, 7) : (s || "");
}

function log(stage, msg, extra = {}) {
  console.log(
    JSON.stringify({
      level: "INFO",
      stage,
      msg,
      pr: `${PR_OWNER}/${PR_REPO}#${PR_NUMBER}`,
      sha: PR_HEAD_SHA,
      ...extra,
    }),
  );
}

function errlog(stage, msg, extra = {}) {
  console.log(
    JSON.stringify({
      level: "ERROR",
      stage,
      msg,
      pr: `${PR_OWNER}/${PR_REPO}#${PR_NUMBER}`,
      sha: PR_HEAD_SHA,
      ...extra,
    }),
  );
}

let statusClient = null;        // Octokit, lazily initialised
let statusCommentId = null;     // id of the comment we PATCH
let statusBy = "";              // who triggered (if known)
let reactableCommentId = null;  // id of the comment to react on failure
let reviewNumber = 1;           // 1-based index of this review run
let previousHeadSHA = null;     // head SHA of the most recent prior Boop summary; null when absent
let botLogin = null;            // GitHub login of the bot App (e.g. "booppr[bot]"); null disables cleanup

async function main() {
  for (const [name, v] of [
    ["GITHUB_APP_ID", GITHUB_APP_ID],
    ["GITHUB_APP_INSTALLATION_ID", GITHUB_APP_INSTALLATION_ID],
    ["GITHUB_APP_PRIVATE_KEY", GITHUB_APP_PRIVATE_KEY],
    ["PR_OWNER", PR_OWNER],
    ["PR_REPO", PR_REPO],
    ["PR_NUMBER", PR_NUMBER],
    ["PR_HEAD_SHA", PR_HEAD_SHA],
    ["PR_BASE_REF", PR_BASE_REF],
    ["OPENROUTER_API_KEY", OPENROUTER_API_KEY],
  ]) {
    if (!v) throw new Error(`missing required env var: ${name}`);
  }

  if (BOOP_STATUS_COMMENT_ID) statusCommentId = Number(BOOP_STATUS_COMMENT_ID);
  if (BOOP_REACTION_COMMENT_ID) reactableCommentId = Number(BOOP_REACTION_COMMENT_ID);
  if (BOOP_REVIEW_NUMBER) {
    const parsed = Number(BOOP_REVIEW_NUMBER);
    if (Number.isInteger(parsed) && parsed >= 1) {
      reviewNumber = parsed;
    }
  }
  if (PR_PREVIOUS_HEAD_SHA && /^[0-9a-f]{7,40}$/.test(PR_PREVIOUS_HEAD_SHA)) {
    previousHeadSHA = PR_PREVIOUS_HEAD_SHA;
  }
  if (BOOP_BOT_LOGIN) botLogin = BOOP_BOT_LOGIN;

  log("start", "boop runner starting", {
    status_comment_id: statusCommentId,
    reaction_comment_id: reactableCommentId,
    review_number: reviewNumber,
    previous_head_sha: previousHeadSHA,
    bot_login: botLogin,
  });

  // Mint installation token first; we need it for the status API too.
  const installationToken = await mintInstallationToken();
  statusClient = new Octokit({ auth: installationToken });
  log("auth", "minted installation token");
  await postStatus("auth");

  await cloneRepo(installationToken);
  log("clone", "repo cloned", { dir: REPO_DIR, sha: PR_HEAD_SHA });
  await postStatus("clone");

  let review;
  try {
    review = await runOpenCodeSkill();
    log("review", "opencode returned", {
      summaryBytes: review.summary.length,
      inlineCount: review.inlineComments.length,
      confidence: review.confidence,
    });
  } catch (err) {
    errlog("review", "opencode failed", { error: String(err?.message ?? err) });
    await postStatus("failed", String(err?.message ?? err));
    throw err;
  }

  await postReview(statusClient, review.summary, reviewNumber, review.confidence);
  log("done", "summary comment posted", {
    review_number: reviewNumber,
    confidence: review.confidence,
  });

  for (const c of review.inlineComments) {
    try {
      await postInlineComment(statusClient, c);
    } catch (err) {
      errlog("inline", "failed to post inline comment", {
        path: c.path,
        line: c.line,
        err: String(err?.message ?? err),
      });
    }
  }
  if (review.inlineComments.length > 0) {
    log("done", `posted ${review.inlineComments.length} inline comments`);
  }

  // On re-reviews, retire prior Boop artifacts so the PR thread looks
  // pristine. Best-effort: any error is logged but the review still
  // completes. Skipped on the first review (nothing to clean) and
  // when BOOP_BOT_LOGIN is unset (the receiver didn't know the bot
  // login — most commonly because the App is configured to omit it).
  if (reviewNumber > 1 && botLogin) {
    try {
      const cleaned = await cleanupPriorReview(installationToken);
      if (cleaned.resolved > 0 || cleaned.minimized > 0) {
        log("cleanup", "retired prior review artifacts", cleaned);
      } else {
        log("cleanup", "no prior artifacts to retire");
      }
    } catch (err) {
      errlog("cleanup", "prior-review cleanup failed", {
        err: String(err?.message ?? err),
      });
    }
  }

  await postStatus("done");
}

async function postStatus(stage, detail) {
  if (!statusClient || !statusCommentId) {
    log("status", "skip (no client or comment id)", { stage });
    return;
  }
  const tpl = STATUS[stage] || `boop status: ${stage}`;
  const short = SHORT[stage] || stage;
  const entry = detail
    ? `- ${short}
  <details><summary>details</summary>

  \`\`\`
  ${detail}
  \`\`\`
  </details>`
    : `- ${short}`;
  try {
    const { data: current } = await statusClient.rest.issues.getComment({
      owner: PR_OWNER,
      repo: PR_REPO,
      comment_id: statusCommentId,
    });
    const sep = "<!-- boop-timeline -->";
    let body;
    if (current.body.includes(sep)) {
      body = current.body + "\n" + entry;
    } else {
      const header = tpl.replace(/\{sha\}/g, shortSha(PR_HEAD_SHA));
      body = `${header}\n\n${sep}\n${entry}`;
    }
    if (body.length > 60000) {
      const cutAt = body.indexOf(sep) + sep.length + 2;
      body = body.slice(0, cutAt) + "_(earlier entries trimmed)_\n" + body.slice(body.length - 58000);
    }
    await statusClient.rest.issues.updateComment({
      owner: PR_OWNER,
      repo: PR_REPO,
      comment_id: statusCommentId,
      body,
    });
    log("status", "comment updated", { stage, comment_id: statusCommentId });
  } catch (err) {
    errlog("status", "comment update failed", { stage, err: String(err?.message ?? err) });
  }
}

async function mintInstallationToken() {
  const now = Math.floor(Date.now() / 1000);
  const appJwt = jwt.sign(
    { iat: now - 30, exp: now + 600, iss: String(GITHUB_APP_ID) },
    GITHUB_APP_PRIVATE_KEY,
    { algorithm: "RS256" },
  );

  const res = await fetch(
    `https://api.github.com/app/installations/${GITHUB_APP_INSTALLATION_ID}/access_tokens`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${appJwt}`,
        Accept: "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "boop-runner",
      },
    },
  );
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`mint installation token: ${res.status} ${text}`);
  }
  const data = await res.json();
  return data.token;
}

async function cloneRepo(token) {
  await fs.rm(REPO_DIR, { recursive: true, force: true });
  await fs.mkdir(REPO_DIR, { recursive: true });
  const url = `https://x-access-token:${token}@github.com/${PR_OWNER}/${PR_REPO}.git`;
  await execFileAsync("git", ["clone", "--depth", "50", url, REPO_DIR], {
    timeout: 5 * 60 * 1000,
  });
  // Fetch every ref the prompt or the LLM might want to `git diff`
  // against. On a re-review with a known prior head, that ref must
  // be present locally so the LLM can run `git diff <prior>...<head>`.
  const fetchRefs = [PR_BASE_REF, PR_HEAD_SHA];
  if (previousHeadSHA && previousHeadSHA !== PR_HEAD_SHA) {
    fetchRefs.push(previousHeadSHA);
  }
  await execFileAsync(
    "git",
    ["fetch", "--depth", "200", "origin", ...fetchRefs],
    { cwd: REPO_DIR, timeout: 5 * 60 * 1000 },
  );
  await execFileAsync("git", ["checkout", PR_HEAD_SHA], { cwd: REPO_DIR });
}

async function runOpenCodeSkill() {
  await materializeConfig();
  const config = JSON.parse(
    await fs.readFile(`${CONFIG_DIR}/opencode.json`, "utf8"),
  );
  const configContent = JSON.stringify(config);

  // For debugging: BOOP_SKIP_SKILL=1 runs a minimal prompt to verify
  // opencode itself works (no skill invocation).
  const prompt = process.env.BOOP_SKIP_SKILL === "1"
    ? `Reply with one sentence: confirm you can see the repo at ${REPO_DIR} on head ${PR_HEAD_SHA.slice(0, 7)}.`
    : await buildBoopPrompt();

  log("opencode", "starting", {
    dir: REPO_DIR,
    model: config.model,
    mode: process.env.BOOP_SKIP_SKILL === "1" ? "minimal" : "full",
  });
  await postStatus("review");

  const { stdout, stderr, code, killed, timeoutMs } = await runOpencode(prompt, configContent);
  log("opencode", "exit", {
    code,
    killed,
    timeoutMs: timeoutMs || 0,
    stdoutBytes: stdout.length,
    stderrBytes: stderr.length,
  });

  if (timeoutMs) {
    throw new Error(`opencode run exceeded ${OPENCODE_TIMEOUT_MS / 60000}-min timeout`);
  }
  if (code !== 0) {
    const tail = stderr.split("\n").slice(-30).join("\n");
    throw new Error(`opencode run exited with code ${code};\n${tail}`);
  }
  if (!stdout.trim()) {
    throw new Error("opencode returned empty stdout");
  }
  const clean = stripAnsi(stdout.trim());

  // Parse the structured SUMMARY / INLINE COMMENTS / CONFIDENCE / END
  // block the model is required to emit. We strip the TUI transcript
  // (everything before the SUMMARY marker) and return:
  //   { summary, inlineComments: [{path, line, body}, ...], confidence }
  // so the caller can post one summary comment + N inline comments
  // via GitHub's PR review comments API, and surface the confidence
  // badge in the summary footer.
  return parseReviewOutput(clean);
}

// parseReviewOutput extracts the structured block from the opencode
// output. Anything before "=== SUMMARY ===" (the TUI prompt, bash
// transcripts, etc.) is dropped. The INLINE COMMENTS section is parsed
// as one "path:line: body" per line. The optional CONFIDENCE section
// is parsed as `high`, `medium`, or `low`; missing or unrecognized
// values default to `medium` so older models keep working.
function parseReviewOutput(output) {
  const summaryMatch = output.match(
    /===\s*SUMMARY\s*===\s*([\s\S]*?)\s*===[\s\S]*?INLINE COMMENTS\s*===\s*([\s\S]*?)\s*===\s*(?:CONFIDENCE\s*===\s*([\s\S]*?)\s*===\s*)?END\s*===/i,
  );
  if (!summaryMatch) {
    // Fallback: no structured block found. Treat the whole output
    // as the summary.
    return { summary: output, inlineComments: [], confidence: "medium" };
  }

  const summary = summaryMatch[1].trim();
  const inlineBlock = summaryMatch[2].trim();
  const confidenceRaw = (summaryMatch[3] || "").trim().toLowerCase();

  const inlineComments = [];
  for (const rawLine of inlineBlock.split("\n")) {
    const line = rawLine.trim();
    if (!line) continue;
    // Match "<path>:<line>: <body>" where path may contain slashes
    // and dots, line is a positive integer, and body is the rest.
    const m = line.match(/^(\S+?):(\d+):\s+(.*)$/);
    if (!m) continue;
    const [, path, lineStr, body] = m;
    const lineNum = Number(lineStr);
    if (!Number.isInteger(lineNum) || lineNum < 1) continue;
    inlineComments.push({ path, line: lineNum, body });
  }

  const confidence = ["high", "medium", "low"].includes(confidenceRaw)
    ? confidenceRaw
    : "medium";

  return { summary, inlineComments, confidence };
}

// stripAnsi removes common ANSI escape sequences so the TUI's
// terminal control codes don't end up in the GitHub review.
function stripAnsi(s) {
  // Matches CSI sequences (\x1b[...m, \x1b[...A/B/C etc.) and OSC (\x1b]...\x07).
  return s
    .replace(/\x1b\][^\x07]*\x07/g, "")
    .replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "")
    .replace(/\x1b[()=>][0-9A-Za-z]/g, "")
    .replace(/[\x00-\x08\x0b-\x1f]/g, "");
}

// buildBoopPrompt reads the boop skill (SKILL.md + every agents/review-*.md)
// directly from the read-only ConfigMap mount (CONFIG_SRC) and inlines them
// into the prompt. opencode-ai's skill discovery doesn't pick up user skills
// from the ConfigMap in this runner setup (only its own built-in
// `customize-opencode` skill loads), so we pre-load the skill content into
// the prompt itself. We deliberately read from the source mount, not from
// the writable copy — cp -rL on the `..data` symlink can pull huge amounts
// of data and OOM the container.
const LENS_FILES = [
  "agents/review-code-quality.md",
  "agents/review-design-pattern.md",
  "agents/review-error-handling.md",
  "agents/review-readability.md",
  "agents/review-solid-principles.md",
  "agents/review-test-quality.md",
  "agents/review-deep.md",
];

async function readWithRetry(path, attempts = 5) {
  let lastErr;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      return await fs.readFile(path, "utf8");
    } catch (err) {
      lastErr = err;
      if (attempt < attempts) {
        await new Promise((r) => setTimeout(r, 1000 * attempt));
      }
    }
  }
  throw lastErr;
}
async function buildBoopPrompt() {
  // Read from the ConfigMap mount directly. The mount uses
  // `..data -> ..2026_...` indirection that can be transiently
  // inconsistent right after pod start, so retry a couple times
  // before giving up.
  const skillPath = `${CONFIG_SRC}/skills/boop/SKILL.md`;
  let skillBody = "";
  for (let attempt = 1; attempt <= 5; attempt++) {
    try {
      skillBody = await fs.readFile(skillPath, "utf8");
      log("skill", "loaded boop SKILL.md", {
        bytes: skillBody.length,
        attempt,
      });
      break;
    } catch (err) {
      log("skill", `read attempt ${attempt} failed`, {
        err: String(err?.message ?? err),
      });
      if (attempt < 5) {
        await new Promise((r) => setTimeout(r, 1000 * attempt));
      }
    }
  }

  // Strip the frontmatter so the model sees a clean system-prompt-ish
  // block instead of duplicate yaml keys.
  const bodyNoFrontmatter = skillBody.replace(/^---[\s\S]*?---\n*/, "");

  // Inline every lens file. The orchestrator (SKILL.md) tells the model to
  // "walk" each lens, but it can't read files in this single-call flow —
  // we have to deliver the content. Order matches LENS_FILES.
  const lensBlocks = [];
  for (const rel of LENS_FILES) {
    const path = `${CONFIG_SRC}/skills/boop/${rel}`;
    try {
      const body = await readWithRetry(path);
      log("skill", "loaded lens", { rel, bytes: body.length });
      const cleaned = body.replace(/^---[\s\S]*?---\n*/, "").trim();
      const label = rel.split("/").pop().replace(/\.md$/, "");
      lensBlocks.push(`### Lens: ${label}\n\n${cleaned}`);
    } catch (err) {
      log("skill", `failed to load lens ${rel}`, {
        err: String(err?.message ?? err),
      });
    }
  }

  // Pick the diff range. On the first review, base..head covers the
  // whole PR. On a re-review with a known prior head, diff the delta
  // from the previously reviewed commit so we don't re-review lines
  // the author already addressed. If the prior SHA is missing (e.g.
  // summaries posted before this feature landed), fall back to the
  // full diff vs base — same as a first review.
  const isReReview = reviewNumber > 1 && previousHeadSHA;
  const diffRange = isReReview
    ? `${previousHeadSHA}...${PR_HEAD_SHA}`
    : `${PR_BASE_REF}...${PR_HEAD_SHA}`;
  const diffHint = isReReview
    ? `Re-review #${reviewNumber}: diff only the delta from the previously reviewed commit ${previousHeadSHA} to ${PR_HEAD_SHA} (do NOT re-review lines from earlier commits — the author has already seen those).`
    : `Compare ${PR_BASE_REF}...${PR_HEAD_SHA} to identify what changed.`;

  return [
    "You are running inside a Kubernetes Job triggered by a GitHub App. " +
      "Review the pull request at the current working directory and produce a " +
      "single summary comment plus line-specific inline comments.",
    "",
    "## Output format (required)",
    "",
    "When you finish, end with EXACTLY this block — the runner parses it:",
    "",
    "=== SUMMARY ===",
    "<one well-formatted Markdown summary of the review>",
    "=== INLINE COMMENTS ===",
    "<empty line, or one inline comment per line in this exact format:>",
    "path/to/file.ext:LINE: <comment body>",
    "path/to/other.ext:LINE: <comment body>",
    "=== CONFIDENCE ===",
    "<high|medium|low — one line, the merge signal>",
    "=== END ===",
    "",
    "Rules:",
    "- The SUMMARY section is what gets posted as a single PR comment.",
    "- Each line in INLINE COMMENTS becomes a line-specific review comment.",
    "- Only include INLINE COMMENTS for genuinely actionable issues. " +
      "A nitpick on every line is noise; surface 3-8 of the most important " +
      "findings.",
    "- line numbers refer to the line in the FILE AS IT APPEARS AFTER the " +
      `diff is applied (the right-hand side in GitHub's diff view). ` +
      `Use \`git diff ${diffRange} -- <file>\` to identify them.`,
    `- Comments must be on lines that were ADDED or MODIFIED in the diff ` +
      `range \`${diffRange}\`. Don't comment on unchanged code.`,
    "- Don't include empty SUMMARY or INLINE COMMENTS sections.",
    "- The CONFIDENCE line is the merge signal: `high` if no Blocking " +
      "findings and full coverage, `medium` if Follow-ups only, `low` " +
      "if any Blocking finding or coverage was incomplete.",
    "",
    "## Skill: boop (orchestrator)",
    "",
    bodyNoFrontmatter.trim(),
    "",
    "## Lenses (read each, apply the checklist, capture findings)",
    "",
    lensBlocks.join("\n\n---\n\n"),
    "",
    "## PR context",
    `- Owner/repo: ${PR_OWNER}/${PR_REPO}`,
    `- PR number: ${PR_NUMBER}`,
    `- Head SHA: ${PR_HEAD_SHA}`,
    isReReview
      ? `- Previous review head SHA: ${previousHeadSHA} (re-review #${reviewNumber} — diff against this, not the base)`
      : `- Base ref: ${PR_BASE_REF}`,
    `- Working directory (already cloned): ${REPO_DIR}`,
    `- ${diffHint}`,
    "",
    "Use the orchestrator and the lenses above to do the actual review. " +
      "When done, emit the SUMMARY / INLINE COMMENTS / CONFIDENCE / END " +
      "block as the LAST thing in your response.",
  ].join("\n");
}

async function materializeConfig() {
  await fs.rm(WRITABLE_HOME, {recursive: true, force: true});
  await fs.rm(WRITABLE_CONFIG, {recursive: true, force: true});
  await fs.mkdir(WRITABLE_CONFIG, {recursive: true});
  // Default cp preserves symlinks. The dest is a copy of the source's
  // symlink tree; the actual file content lives behind the symlinks
  // and is read directly from the source mount wherever possible.
  // We don't need to dereference (cp -rL) — that would pull every
  // previous ConfigMap version into the dest and OOM the pod.
  await execFileAsync("cp", ["-r", `${CONFIG_SRC}/.`, `${CONFIG_DIR}/`]);
}

function runOpencode(prompt, configContent) {
  return new Promise((resolve) => {
    // The opencode CLI is a TUI (Bubble Tea) and the `run` subcommand
    // still goes through some of the same init paths. In a K8s pod
    // stdin is /dev/null and there's no controlling terminal, which
    // makes the binary hang at the `init` log. The official
    // opencode-agent GitHub App works because it runs in a GitHub
    // Actions runner with a TTY.
    //
    // We work around it by wrapping the invocation in `script -qfc`
    // which allocates a pseudo-tty. Combined with `--auto` and
    // `--print-logs`, the binary boots headless on a PTY, runs the
    // prompt, and writes the assistant response to the pty master
    // (which Node reads as stdout).
    const cmd = [
      "script",
      "-qfc",
      [
        "opencode",
        "run",
        shellQuote(REPO_DIR),
        shellQuote(prompt),
        "--auto",
        ...(process.env.BOOP_DEBUG ? ["--log-level", "DEBUG", "--print-logs"] : []),
      ].join(" "),
      "/dev/null",
    ];
    log("opencode", "spawning", { via: "script(1) PTY", flagCount: cmd.length });

    const proc = spawn(cmd[0], cmd.slice(1), {
      env: {
        ...process.env,
        HOME: WRITABLE_HOME,
        XDG_CONFIG_HOME: WRITABLE_CONFIG,
        OPENCODE_CONFIG_CONTENT: configContent,
        OPENCODE_CONFIG_DIR: CONFIG_DIR,
        // Make sure TERM is set so the TUI can pick a sane mode.
        TERM: process.env.TERM || "xterm-256color",
      },
    });

    let stdout = "";
    let stderr = "";
    let killed = false;
    let timeoutMs = 0;

    const timer = setTimeout(() => {
      killed = true;
      timeoutMs = OPENCODE_TIMEOUT_MS;
      errlog("opencode", "killing subprocess after timeout", { ms: OPENCODE_TIMEOUT_MS });
      proc.kill("SIGKILL");
    }, OPENCODE_TIMEOUT_MS);

    proc.stdout.on("data", (chunk) => {
      stdout += chunk.toString();
    });
    proc.stderr.on("data", (chunk) => {
      const text = chunk.toString();
      stderr += text;
      for (const line of text.split("\n")) {
        if (line.trim()) errlog("opencode-stderr", line);
      }
    });
    proc.on("error", (err) => {
      clearTimeout(timer);
      resolve({ stdout, stderr, code: -1, killed, timeoutMs });
    });
    proc.on("close", (code) => {
      clearTimeout(timer);
      resolve({ stdout, stderr, code, killed, timeoutMs });
    });
  });
}

// confidenceBadge renders Boop's merge-signal badge for the summary
// footer. Keep the visual cues (✅ / ⚠️ / 🚨) and the wording aligned
// with the table in apps/k8s/base/runner-config/skills/boop/SKILL.md
// ("Confidence line" subsection).
function confidenceBadge(c) {
  switch (c) {
    case "high":
      return "✅ **Confidence: high** — ready to merge.";
    case "medium":
      return "⚠️ **Confidence: medium** — Follow-ups worth addressing, no Blocking findings.";
    case "low":
    default:
      return "🚨 **Confidence: low** — Blocking finding(s) present, not safe to merge without changes.";
  }
}

async function postReview(octokit, body, reviewNumber, confidence) {
  const max = 65000;
  const cleaned = body.replace(/\n{3,}/g, "\n\n").trim();
  const trimmed = cleaned.length > max ? cleaned.slice(0, max - 50) + "\n\n…(truncated)" : cleaned;
  const reviewTag = reviewNumber > 1 ? ` · review #${reviewNumber}` : "";
  const badge = confidenceBadge(confidence || "medium");
  // Hidden marker carrying the full head SHA so the next re-review
  // can diff the delta from this commit. The receiver parses this
  // (see priorReviewHeadSHARegex in client.go). GitHub renders HTML
  // comments as nothing in the markdown view, so it's invisible to
  // human readers.
  const headMarker = `<!-- boop-head-sha: ${PR_HEAD_SHA} -->`;
  await octokit.rest.issues.createComment({
    owner: PR_OWNER,
    repo: PR_REPO,
    issue_number: Number(PR_NUMBER),
    body:
      `${reviewHeader(reviewNumber)}\n\n` +
      `${badge}\n\n` +
      trimmed +
      `\n\n<sub>Posted by [BoopPr](https://github.com/qubitquilt/boop) · PR \`${shortSha(PR_HEAD_SHA)}\`${reviewTag} · good boy powered</sub>` +
      `\n${headMarker}`,
  });
}

// postInlineComment creates a single line-specific review comment
// on the PR. Uses GitHub's "review comments" API which renders
// inline on the file diff. Each comment is independent (a "pending
// review" with one comment), so even if some fail, the rest still
// post.
async function postInlineComment(octokit, c) {
  await octokit.rest.pulls.createReviewComment({
    owner: PR_OWNER,
    repo: PR_REPO,
    pull_number: Number(PR_NUMBER),
    body: c.body,
    commit_id: PR_HEAD_SHA,
    path: c.path,
    line: c.line,
    side: "RIGHT",
  });
}

// graphql POSTs a query/mutation to api.github.com/graphql with the
// installation token and returns the JSON `data` object. Throws on
// transport errors or top-level GraphQL errors.
async function graphql(token, query, variables) {
  const res = await fetch("https://api.github.com/graphql", {
    method: "POST",
    headers: {
      Authorization: `bearer ${token}`,
      "Content-Type": "application/json",
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": "2022-11-28",
      "User-Agent": "boop-runner",
    },
    body: JSON.stringify({ query, variables }),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`graphql HTTP ${res.status}: ${text.slice(0, 200)}`);
  }
  const json = await res.json();
  if (json.errors && json.errors.length > 0) {
    throw new Error(`graphql: ${json.errors.map((e) => e.message).join("; ")}`);
  }
  return json.data;
}

// paginateThreads walks every review thread on the PR, paginating
// until exhausted. Each returned thread is annotated with the
// original comment's author login (case-insensitive match).
async function fetchAllReviewThreads(token) {
  const threads = [];
  let cursor = null;
  const query = `
    query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
      repository(owner: $owner, name: $repo) {
        pullRequest(number: $number) {
          reviewThreads(first: 100, after: $cursor) {
            pageInfo { hasNextPage endCursor }
            nodes {
              id
              isResolved
              isOutdated
              comments(first: 1) {
                nodes { author { login } }
              }
            }
          }
        }
      }
    }`;
  while (true) {
    const data = await graphql(token, query, {
      owner: PR_OWNER,
      repo: PR_REPO,
      number: Number(PR_NUMBER),
      cursor,
    });
    const conn = data?.repository?.pullRequest?.reviewThreads;
    if (!conn) break;
    for (const node of conn.nodes) {
      const author =
        node?.comments?.nodes?.[0]?.author?.login?.toLowerCase() || "";
      threads.push({
        id: node.id,
        isResolved: node.isResolved === true,
        isOutdated: node.isOutdated === true,
        author,
      });
    }
    if (!conn.pageInfo?.hasNextPage) break;
    cursor = conn.pageInfo.endCursor;
  }
  return threads;
}

// fetchAllIssueCommentIDs walks every issue comment on the PR via
// the REST API (GraphQL's pullRequest.comments misses some bot
// comments that were posted via the issue-comments API). Returns
// the integer IDs of every comment posted by the bot, excluding
// the current run's status comment.
async function fetchPriorBotIssueCommentIDs(token) {
  const ids = [];
  let page = 1;
  const url = `https://api.github.com/repos/${PR_OWNER}/${PR_REPO}/issues/${PR_NUMBER}/comments?per_page=100&page=${page}`;
  const headers = {
    Authorization: `bearer ${token}`,
    Accept: "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "boop-runner",
  };
  while (true) {
    const res = await fetch(`${url.split("?")[0]}?per_page=100&page=${page}`, { headers });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`list comments HTTP ${res.status}: ${text.slice(0, 200)}`);
    }
    const arr = await res.json();
    if (!Array.isArray(arr) || arr.length === 0) break;
    for (const c of arr) {
      const author = c?.user?.login?.toLowerCase() || "";
      if (author !== botLogin.toLowerCase()) continue;
      if (statusCommentId && Number(c.id) === statusCommentId) continue;
      ids.push({ id: Number(c.id), nodeId: c.node_id });
    }
    if (arr.length < 100) break;
    page++;
  }
  return ids;
}

// resolveReviewThread marks one review thread as resolved.
async function resolveReviewThread(token, threadId) {
  const data = await graphql(
    token,
    `mutation($id: ID!) {
       resolveReviewThread(input: { threadId: $id }) {
         thread { id isResolved }
       }
     }`,
    { id: threadId },
  );
  return data?.resolveReviewThread?.thread?.isResolved === true;
}

// minimizeComment collapses a comment in the PR UI. The body
// stays in the API (so the boop-head-sha marker remains parsable
// by the receiver's CountPriorReviews) but the comment is hidden
// by default.
async function minimizeComment(token, commentNodeId) {
  const data = await graphql(
    token,
    `mutation($id: ID!) {
       minimizeComment(input: { subjectId: $id, classifier: OUTDATED }) {
         minimizedComment { isMinimized }
       }
     }`,
    { id: commentNodeId },
  );
  return data?.minimizeComment?.minimizedComment?.isMinimized === true;
}

// cleanupPriorReview runs on re-reviews only. It:
//   1. Resolves every Boop review thread whose diff line is gone
//      or changed (isOutdated === true) — the author has either
//      fixed the issue or removed the code.
//   2. Minimizes every other prior Boop issue comment (status
//      threads, prior summary comments) so the PR UI is dominated
//      by the active review.
//
// Best-effort. The review already posted — a cleanup failure is
// logged but does not fail the run.
async function cleanupPriorReview(token) {
  const result = { resolved: 0, minimized: 0, errors: 0 };

  // 1. Resolve outdated bot review threads.
  const threads = await fetchAllReviewThreads(token);
  const targets = threads.filter(
    (t) => !t.isResolved && t.isOutdated && t.author === botLogin.toLowerCase(),
  );
  log("cleanup", "scanned review threads", {
    total: threads.length,
    bot_outdated: targets.length,
  });
  for (const t of targets) {
    try {
      if (await resolveReviewThread(token, t.id)) {
        result.resolved++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "resolve failed", {
        thread: t.id,
        err: String(err?.message ?? err),
      });
    }
  }

  // 2. Minimize every prior bot issue comment.
  const priors = await fetchPriorBotIssueCommentIDs(token);
  log("cleanup", "scanned issue comments", { bot_total: priors.length });
  for (const c of priors) {
    try {
      if (await minimizeComment(token, c.nodeId)) {
        result.minimized++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "minimize failed", {
        comment: c.id,
        err: String(err?.message ?? err),
      });
    }
  }

  return result;
}

main().catch(async (err) => {
  errlog("fatal", "boop runner failed", { error: String(err?.message ?? err) });
  if (statusClient && statusCommentId) await postStatus("failed", String(err?.message ?? err));
  process.exit(1);
});

// shellQuote returns a string safe to embed in a single-quoted
// shell argument (closes any embedded single quotes).
function shellQuote(s) {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}
