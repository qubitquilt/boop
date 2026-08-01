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

export const REPO_DIR = "/work/repo";

// Source of the read-only opencode config ConfigMap (boop-runner-config).
// The runner reads from this mount directly (not the materialized copy)
// to avoid pulling every prior ConfigMap version into the writable
// destination and OOMing the pod.
export const CONFIG_SRC = "/home/opencode/.config/opencode";

export const WRITABLE_HOME = "/tmp/opencode-home";
export const WRITABLE_CONFIG = "/tmp/opencode-config";
export const CONFIG_DIR = `${WRITABLE_CONFIG}/opencode`;

export const NETRC_PATH = "/tmp/boop-netrc";
export const GITCONFIG_PATH = "/tmp/boop-gitconfig";

// Default secret mount paths. Overridable via env for tests and for
// future relocations; the receiver's Job template currently mounts at
// these locations.
export const DEFAULT_PRIVATE_KEY_PATH = "/secrets/github-app-private-key";
export const DEFAULT_OPENROUTER_KEY_PATH = "/secrets/openrouter-api-key";

// Hard ceiling on the opencode subprocess. The Job has a 30-min
// activeDeadlineSeconds; keep some headroom for the post-review work.
export const OPENCODE_TIMEOUT_MS = 25 * 60 * 1000;

// Status stages. The receiver uses the same vocabulary so the user
// can correlate GitHub comment updates with runner log lines.
export const STATUS = {
  initial: "🐾 **Boop's on the case!** Digging in at `{sha}`. Updates will appear here.",
  auth: "🤝 **Paw-shaken in** — authenticated with GitHub at `{sha}`.",
  clone: "🥎 **Boop fetched the repo** at `{sha}`. Checking out the PR head and starting the multi-lens review.",
  review: "👃 **Boop is sniffing** — running the multi-lens review on `{sha}`.",
  done: "💤 **Boop napped.** See the comment below.",
  failed: "🔄 **Boop chased his tail.** Check the Job logs for details.",
};

// Short labels used in the timeline. The header above always shows
// the full state; the timeline is a one-line-per-stage log.
export const SHORT = {
  auth: "🤝 paw-shaken in",
  clone: "🥎 fetched",
  review: "👃 sniffing",
  done: "💤 napped",
  failed: "🔄 chased tail",
};

// Lens files inlined into the prompt. Order matches the order the
// orchestrator tells the model to walk them. Read from the source
// mount (not the materialized copy) — see `CONFIG_SRC` comment.
export const LENS_FILES = [
  "agents/review-code-quality.md",
  "agents/review-design-pattern.md",
  "agents/review-error-handling.md",
  "agents/review-readability.md",
  "agents/review-solid-principles.md",
  "agents/review-test-quality.md",
  "agents/review-deep.md",
];

// --- loadConfig ----------------------------------------------------------

// `env` is the parsed process.env (or a fixture). The returned object
// is plain data; downstream functions never reach back into env.
export function loadConfig(env = process.env) {
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
    prOwner: env.PR_OWNER,
    prRepo: env.PR_REPO,
    prNumber: env.PR_NUMBER,
    prHeadSha: env.PR_HEAD_SHA,
    prBaseRef: env.PR_BASE_REF,
    previousHeadSha: env.PR_PREVIOUS_HEAD_SHA || null,
    githubAppId: env.GITHUB_APP_ID,
    githubAppInstallationId: env.GITHUB_APP_INSTALLATION_ID,
    privateKeyPath: env.BOOP_GITHUB_APP_PRIVATE_KEY_PATH || DEFAULT_PRIVATE_KEY_PATH,
    openrouterKeyPath: env.BOOP_OPENROUTER_API_KEY_PATH || DEFAULT_OPENROUTER_KEY_PATH,
    statusCommentId: parsePositiveInt(env.BOOP_STATUS_COMMENT_ID),
    reactionCommentId: parsePositiveInt(env.BOOP_REACTION_COMMENT_ID),
    reviewNumber: parsePositiveInt(env.BOOP_REVIEW_NUMBER) || 1,
    botLogin: env.BOOP_BOT_LOGIN || null,
    skipSkill: env.BOOP_SKIP_SKILL === "1",
    debug: !!env.BOOP_DEBUG,
    home: env.HOME || "/home/opencode",
    cwd: env.CWD || "/app",
    // Dashboard data layer. Both must be set for the runner to
    // POST lifecycle + telemetry; if either is missing the
    // dashboard helpers are no-ops and the runner falls back
    // to the TUI mode in opencode.mjs (no telemetry captured).
    dashboardUrl: env.BOOP_DASHBOARD_URL || null,
    dashboardToken: env.BOOP_DASHBOARD_TOKEN || null,
    jobName: env.BOOP_JOB_NAME || null,
  };
}

function parsePositiveInt(v) {
  if (v === undefined || v === null || v === "") return null;
  const n = Number(v);
  if (!Number.isInteger(n) || n < 1) return null;
  return n;
}
