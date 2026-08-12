// Path constants, defaults, and `loadConfig` for the runner.
//
// The config object is the single source of truth threaded through
// every step of the pipeline. Pure functions (assertSafeRef, etc.)
// don't read it; side-effecting ones (cloneRepo, postStatus) accept it
// as their first arg.
//
// Why env-as-arg, not read-at-module-load: the runner's tests used to
// be unable to vary inputs without polluting `process.env`, and the
// pipeline's modules all reached into module-level state on every
// call. By making loadConfig a pure function over `env`, every later
// step is independently testable with a fixture ctx.

// --- paths ---------------------------------------------------------------

// QUB-<next> Option A local-run support: production runs on a
// K8s Job that mounts the cloned repo at /work/repo and the
// runner-config ConfigMap at /home/opencode/.config/opencode.
// Both paths are sealed on macOS workstations, so the runner
// accepts BOOP_REPO_DIR / BOOP_CONFIG_SRC env overrides for
// local dev. Defaults preserve the production contract.
//
// repoDir / configSrc flow through loadConfig like every other
// config field (they no longer read process.env at module load,
// so tests vary them via the env fixture the same way they vary
// PR_OWNER).
export const NETRC_PATH = "/tmp/boop-netrc";
export const GITCONFIG_PATH = "/tmp/boop-gitconfig";

// Default secret mount paths. Overridable via env for tests and for
// future relocations; the receiver's Job template currently mounts at
// these locations.
export const DEFAULT_PRIVATE_KEY_PATH = "/secrets/github-app-private-key";
export const DEFAULT_OPENROUTER_KEY_PATH = "/secrets/openrouter-api-key";

// Hard ceiling on the OpenRouter SDK call. The Job has a 30-min
// activeDeadlineSeconds; keep some headroom for the post-review work.
// The constant name is preserved (it predates the SDK cutover) so
// dashboards, dashboards-side tests, and receivers that key off the
// 25-min budget don't have to update.
export const OPENCODE_TIMEOUT_MS = 25 * 60 * 1000;

// Status stages. The receiver uses the same vocabulary so the user
// can correlate GitHub comment updates with runner log lines.
export const STATUS: Record<string, string> = {
  initial: "🐾 **Boop's on the case!** Digging in at `{sha}`. Updates will appear here.",
  auth: "🤝 **Paw-shaken in** — authenticated with GitHub at `{sha}`.",
  clone: "🥎 **Boop fetched the repo** at `{sha}`. Checking out the PR head and starting the multi-lens review.",
  review: "👃 **Boop is sniffing** — running the multi-lens review on `{sha}`.",
  done: "🦴 **Boop brought you a bone.** See the comment below.",
  failed: "❌ **Boop lost the bone.** Check the Job logs for details.",
};

// Short labels used in the timeline. The header above always shows
// the full state; the timeline is a one-line-per-stage log.
export const SHORT: Record<string, string> = {
  auth: "🤝 paw-shaken in",
  clone: "🥎 fetched",
  review: "👃 sniffing",
  done: "🦴 bone delivered",
  failed: "❌ lost the bone",
};

// Lens files inlined into the prompt. Order matches the order the
// orchestrator tells the model to walk them. Read from the config
// mount (ctx.configSrc)/skills/boop/.
export const LENS_FILES = [
  "agents/review-code-quality.md",
  "agents/review-design-pattern.md",
  "agents/review-error-handling.md",
  "agents/review-readability.md",
  "agents/review-solid-principles.md",
  "agents/review-test-quality.md",
  "agents/review-deep.md",
] as const;

// --- loadConfig ----------------------------------------------------------

// `env` is the parsed process.env (or a fixture). The returned object
// is plain data; downstream functions never reach back into env.
export function loadConfig(env: NodeJS.ProcessEnv = process.env): {
  prOwner: string;
  prRepo: string;
  prNumber: string;
  prHeadSha: string;
  prBaseRef: string;
  previousHeadSha: string | null;
  repoDir: string;
  configSrc: string;
  githubAppId: string;
  githubAppInstallationId: string;
  privateKeyPath: string;
  openrouterKeyPath: string;
  statusCommentId: number | null;
  reactionCommentId: number | null;
  reviewNumber: number;
  botLogin: string | null;
  triggeredBy: string | null;
  skipSkill: boolean;
  debug: boolean;
  home: string;
  cwd: string;
  noStatusComment: boolean;
  rtkDisabled: boolean;
  dashboardUrl: string | null;
  dashboardToken: string | null;
  jobName: string | null;
  parentRunId: string | null;
  openrouterModel: string | null;
  toolsEnabled: boolean;
} {
  const required = [
    "PR_OWNER",
    "PR_REPO",
    "PR_NUMBER",
    "PR_HEAD_SHA",
    "PR_BASE_REF",
  ];
  for (const name of required) {
    if (!env[name]) {
      throw new Error(`missing required env var: ${name}`);
    }
  }
  if (!env.GITHUB_APP_ID) throw new Error("missing required env var: GITHUB_APP_ID");
  if (!env.GITHUB_APP_INSTALLATION_ID) {
    throw new Error("missing required env var: GITHUB_APP_INSTALLATION_ID");
  }

  return {
    prOwner: env.PR_OWNER as string,
    prRepo: env.PR_REPO as string,
    prNumber: env.PR_NUMBER as string,
    prHeadSha: env.PR_HEAD_SHA as string,
    prBaseRef: env.PR_BASE_REF as string,
    previousHeadSha: env.PR_PREVIOUS_HEAD_SHA || null,
    // Local-run override (Option A): the clone mount and the
    // config mount are /work/repo and
    // /home/opencode/.config/opencode in production; the env
    // overrides unseal them on macOS workstations.
    repoDir: env.BOOP_REPO_DIR || "/work/repo",
    configSrc: env.BOOP_CONFIG_SRC || "/home/opencode/.config/opencode",
    githubAppId: env.GITHUB_APP_ID as string,
    githubAppInstallationId: env.GITHUB_APP_INSTALLATION_ID as string,
    privateKeyPath:
      env.BOOP_GITHUB_APP_PRIVATE_KEY_PATH || DEFAULT_PRIVATE_KEY_PATH,
    openrouterKeyPath:
      env.BOOP_OPENROUTER_API_KEY_PATH || DEFAULT_OPENROUTER_KEY_PATH,
    statusCommentId: parsePositiveInt(env.BOOP_STATUS_COMMENT_ID),
    reactionCommentId: parsePositiveInt(env.BOOP_REACTION_COMMENT_ID),
    reviewNumber: parsePositiveInt(env.BOOP_REVIEW_NUMBER) || 1,
    botLogin: env.BOOP_BOT_LOGIN || null,
    // QUB-99: GitHub login of the user who triggered an
    // issue_comment-based review. Forwarded by the receiver
    // from the issue_comment payload via BOOP_SENDER_LOGIN so
    // the runner can render the "Triggered by @user"
    // attribution on the initial status comment it creates on
    // its first postStatus call. Empty for pull_request-driven
    // runs.
    triggeredBy: env.BOOP_SENDER_LOGIN || null,
    skipSkill: env.BOOP_SKIP_SKILL === "1",
    debug: !!env.BOOP_DEBUG,
    home: env.HOME || "/home/opencode",
    cwd: env.CWD || "/app",
    // QUB-114: when an issue_comment triggers a review, the
    // receiver already reacts with 👀 to the trigger comment.
    // The runner should NOT post a status comment + PATCH it
    // for every stage — that dings the author on every update.
    // Instead the runner adds a single terminal reaction
    // (🦴 on done, ❌ on failed) to the trigger comment and
    // postStatus is a no-op. The trigger is silent otherwise.
    noStatusComment: env.BOOP_NO_STATUS_COMMENT === "1",
    // QUB-85: operator kill switch for the rtk adapter. When `1`,
    // the adapter skips the rtk binary and reads go through raw
    // `fs.readFile` — the pre-QUB-85 behavior. Use this to
    // reproduce a regression or to debug a poisoned rtk install
    // without rebuilding the image.
    rtkDisabled: env.BOOP_RTK_DISABLED === "1",
    // Dashboard data layer. Both must be set for the runner to
    // POST lifecycle + telemetry; if either is missing the
    // dashboard helpers are no-ops and the runner still posts the
    // review to GitHub (telemetry is simply not captured).
    dashboardUrl: env.BOOP_DASHBOARD_URL || null,
    dashboardToken: env.BOOP_DASHBOARD_TOKEN || null,
    jobName: env.BOOP_JOB_NAME || null,
    // QUB-110: parent-run id. Set by the receiver's
    // re-run jobbuilder when this Job is a re-run
    // of an earlier run. The runner uses it to
    // populate the prompt's PRIOR_RUN_CONTEXT block
    // so the model is aware a previous review exists
    // and should not re-flag issues already addressed.
    // Empty on the first review of a PR.
    parentRunId: env.BOOP_PARENT_RUN_ID || null,
    // Model name forwarded to the OpenRouter SDK. The QUB-94
    // cutover used opencode.json's `model` field as the
    // fallback; QUB-98 deleted the opencode.json ConfigMap so
    // this env var is now the only source of the model name.
    openrouterModel: env.OPENROUTER_MODEL || null,
    // QUB-132: agent tool-set kill switch. The QUB-132 SDK
    // swap gave the reviewer a tool set (run_command, read_file,
    // git_diff) for the experts + narrator. Set
    // BOOP_TOOLS_ENABLED=0 to disable tools fleet-wide (the
    // narrator gets the QUB-130 "no tools available" prompt
    // variant and a no-tools SDK call). The walkthrough is
    // unaffected — it never had tools. Default `true` keeps
    // tool execution on for normal operations; this env var is
    // the operator's escape hatch when a tool-using prompt
    // regresses or a runner host can't safely execute
    // subprocesses.
    toolsEnabled: env.BOOP_TOOLS_ENABLED !== "0",
  };
}

function parsePositiveInt(v: string | undefined | null): number | null {
  if (v === undefined || v === null || v === "") return null;
  const n = Number(v);
  if (!Number.isInteger(n) || n < 1) return null;
  return n;
}
