/**
 * Shared types for the Boop runner (QUB-136).
 *
 * These five types are the contract every stage + lib function
 * reads. Defining them once here keeps the macro + sub stage
 * table in workflow.ts compile-time correct and pushes the
 * state / deps / ctx shape into the type-checker.
 *
 * Style notes:
 *  - Most fields are optional because every stage only reads
 *    a subset; making them all required would force every
 *    function that constructs a partial ctx to populate fields
 *    it does not use. The lib functions defensively check
 *    before reading, so the optional shape matches the runtime
 *    contract.
 *  - `unknown` is used in place of `any` wherever a tighter
 *    type would force a refactor; the lib code's defensive
 *    checks (the same checks the production code does today)
 *    narrow the type at the read site.
 *  - `Ctx` carries the narrate stage's `walkthrough` and
 *    `findings` keys so the prompt builder can read them
 *    without a separate type.
 */

// --- Ctx ----------------------------------------------------------------

/**
 * Ctx: the PR context threaded through every stage.
 *
 * loadConfig() in lib/config.ts produces the base shape. The
 * orchestrator (index.ts) mutates reviewId after the run starts.
 * The narrate sub-stage augments it with walkthrough + findings
 * when calling the LLM.
 */
export type Ctx = {
  prOwner: string;
  prRepo: string;
  prNumber: string | number;
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
  reviewId?: string;
  // Narration augmentation: the narrate sub-stage injects the
  // walkthrough + findings so the LLM prompt can consume them
  // without re-reading from state.
  walkthrough?: string;
  findings?: Finding[];
  // Optional: pre-computed diff range forwarded by the caller.
  diffRange?: string;
  // Optional: paths is sometimes attached by test fixtures
  // (production wires paths through deps, not ctx). Accept the
  // shape defensively.
  paths?: { repoDir: string; configSrc: string };
};

// --- Deps ---------------------------------------------------------------

/**
 * Deps: the DI bag every lib function reads.
 *
 * Two flavours in practice: a full production deps (built in
 * makeDeps in index.ts) and a minimal test deps (a recording
 * object in workflow.test.ts). The shared fields are listed
 * first; the conditional fields below are null in tests.
 */
export type Deps = {
  // File system + subprocess
  fs: FsLike;
  execFile: ExecFileLike;
  spawnFn?: SpawnFn;

  // GitHub / Octokit
  jwt: JwtLike;
  fetchImpl: FetchLike;
  OctokitCtor?: OctokitCtorLike;

  // Environment snap
  env: { OPENROUTER_API_KEY: string } & Record<string, string>;

  // Paths
  paths: {
    repoDir: string;
    configSrc: string;
    netrc: string;
    gitconfig: string;
  };

  // Cleanup
  cleanup: CleanupRegistry;

  // Logging
  log: (stage: string, msg: string, extra?: Record<string, unknown>) => void;
  errlog: (stage: string, msg: string, extra?: Record<string, unknown>) => void;

  // Octokit slot
  setOctokit: (octokit: OctokitLike | null) => void;
  getOctokit: () => OctokitLike | null;

  // Ctx helper (QUB-99)
  currentCtx: () => Ctx;

  // Status
  postStatus: (stage: string, detail?: string) => Promise<void>;

  // Stage helpers
  cloneRepo: (token: string, ctx: Ctx, deps: Deps) => Promise<void>;
  expertOverrides?: Record<string, ExpertFn>;
  generateWalkthrough?: (
    ctx: Ctx,
    deps: Deps,
  ) => Promise<{ walkthrough: string; telemetry: Telemetry | null }>;
  sleep?: (ms: number) => Promise<void>;
  // Pre-computed diff range forwarded by the caller (e.g. the
  // prompt builder) so the git_diff tool does not have to
  // re-derive it from ctx.
  diffRange?: string;
  // Per-stage POST helper (QUB-109 waterfall). Optional; the
  // dashboard.ts default is used when the runner does not
  // attach a custom hook.
  postStage?: (
    stageName: string,
    ctx: Ctx,
    deps: Deps,
    opts?: { ended?: boolean; meta?: Record<string, unknown> },
  ) => void | Promise<void>;
  // Per-read retry overrides for the SKILL / lens loaders.
  // The default values live in prompt.ts; tests inject lower
  // counts to exercise the failure path immediately.
  retries?: { skill: number; lens: number };
  // Walkthrough model override. Production uses ctx.openrouterModel;
  // an operator can force a specific model for the walkthrough via
  // this dep.
  walkthroughModel?: string;
  // Underlying fetch. The graphQL helper in github/auth
  // accepts either `fetch` (the default) or `fetchImpl`
  // (the override); production wires the global fetch and
  // tests inject a fake.
  fetch: FetchLike;

  // Retry policy
  stageMaxAttempts?: number;
  stageBackoffBaseMs?: number;
  stageBackoffMaxMs?: number;

  // Test seam — callOpenRouter override for the SDK call
  callOpenRouter?: (prompt: string, deps: CallDeps) => Promise<CallResult>;

  // RTK adapter (QUB-85)
  rtk?: RtkAdapter;
};

// --- State --------------------------------------------------------------

/**
 * State: the shared state every stage mutates.
 *
 * The orchestrator owns the State object; the stages read and
 * write fields on it. The fields are populated lazily (the
 * handshake stage writes installationToken + openrouterApiKey +
 * octokit; the walkthrough sub-stage writes walkthrough; etc.)
 * so any given stage reads only a subset.
 */
export type State = {
  // Auth
  installationToken?: string;
  openrouterApiKey?: string;
  octokit?: OctokitLike;

  // Walkthrough
  walkthrough?: string;
  walkthroughTelemetry?: Telemetry | null;

  // Classification
  classification?: { type: string; confidence: string };

  // Dispatch + meta-review
  findings?: Finding[];
  lensTelemetry?: LensTelemetry[];

  // Review (summary + inlines + confidence)
  review?: Review;

  // QUB-114: bot login
  botLogin?: string;

  // Cleanup result
  cleanup?: { resolved: number; minimized: number; errors: number } | null;

  // QUB-92 resume: list of macro stage ids that have already
  // passed in a prior pod.
  passed?: string[];
  // Per-macro sub-stage passed list.
  sub?: Record<string, string[]>;

  // Marker set by the sniff macro-stage so the sub-executor
  // knows which per-macro skip list to apply.
  _subWorkflowOf?: string;

  // Soft-fail flag (a gate or abort path set it).
  parseFailed?: boolean;
  failureReason?: string;
};

// --- Overrides ----------------------------------------------------------

/**
 * Overrides: the per-stage test seam (workflow.ts reads these
 * off the third run arg). All fields optional.
 */
export type Overrides = {
  makeOctokit?: (token: string) => OctokitLike;
  runOpenCodeSkill?: RunOpenCodeSkillFn;
  classify?: (ctx: Ctx, deps: Deps) => Promise<Classification>;
  narrate?: (
    findings: Finding[],
    ctx: Ctx,
    deps: Deps,
  ) => Promise<Review>;
  gather?: (findings: Finding[]) => Finding[];
  metaReview?: MetaReviewFn;
  pickExperts?: (classification: Classification) => string[];
  runExperts?: (
    names: string[],
    ctx: Ctx,
    deps: Deps,
    shared?: ExpertShared,
  ) => Promise<ExpertResult | Finding[]>;
  generateWalkthrough?: (
    ctx: Ctx,
    deps: Deps,
  ) => Promise<{ walkthrough: string; telemetry: Telemetry | null }>;
  cleanupPriorReview?: (token: string, ctx: Ctx, deps: Deps) => Promise<CleanupResult>;
  postStage?: (stageName: string, ctx: Ctx, deps: Deps, opts?: { ended?: boolean; meta?: Record<string, unknown> }) => void;
};

// --- Stage --------------------------------------------------------------

/**
 * Stage<Id>: the macro + sub contract. Each stage is a data
 * record the executor walks; the gate function decides whether
 * the stage proceeds, the run function performs the work.
 *
 * `Id` is the string-literal type for the stage id; the
 * `as const satisfies readonly Stage[]` annotation on STAGES +
 * REVIEW_SUB_STAGES in workflow.ts pins the macro order and
 * the id literals at compile time.
 */
export type Stage<Id extends string = string> = {
  id: Id;
  // Pinned by QUB-93; null = silent on the status thread.
  statusStage: string | null;
  description: string;
  input: string;
  output: string;
  idempotent: boolean;
  retryable: boolean;
  gate: (state: State, ctx: Ctx, deps: Deps) => Promise<GateResult>;
  run: (ctx: Ctx, deps: Deps, overrides: Overrides, state: State) => Promise<void>;
};

export type GateResult =
  | { ok: true }
  | { ok: false; reason: string };

// --- Finding + Review ---------------------------------------------------

export type Finding = {
  id: string;
  expert: string;
  severity: "blocking" | "follow-up" | "optional" | "info";
  title: string;
  body: string;
  path?: string;
  line?: number;
};

export type Review = {
  summary: string;
  inlineComments: InlineComment[];
  confidence: "high" | "medium" | "low";
  parseError?: string | null;
  telemetry?: Telemetry | null;
};

export type InlineComment = { path: string; line: number; body: string };

export type Classification = { type: string; confidence: string };

// --- Telemetry ----------------------------------------------------------

export type Telemetry = {
  model: string;
  provider: string;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  reasoningTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  costUsd: number;
  costPromptUsd: number;
  costCompletionUsd: number;
  costUpstreamUsd: number;
  isByok: boolean;
  serverToolCallsExecuted: number;
  serverToolCallsRequested: number;
  requestId?: string;
  durationMs?: number;
  stepCount: number;
  error?: string;
  errorStatusCode?: number;
  errorContentType?: string;
  errorBody?: string;
};

export type LensTelemetry = {
  lens: string;
  model: string;
  provider: string;
  inputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  costUsd: number;
  stepCount: number;
};

// --- Expert / shared ----------------------------------------------------

export type ExpertFn = (
  ctx: Ctx,
  deps: Deps,
  shared?: ExpertShared,
) => Promise<ExpertResult>;

export type ExpertShared = {
  classification?: Classification;
  walkthrough?: string;
  findings?: Finding[];
};

export type ExpertResult = {
  findings: Finding[];
  telemetry?: Telemetry | LensTelemetry | null;
};

export type MetaReviewFn = (
  findings: Finding[],
  classification: Classification,
  ctx: Ctx,
  deps: Deps,
) => Promise<{ reDispatch: string[] }>;

export type CleanupResult = { resolved: number; minimized: number; errors: number };

// --- OpenRouter SDK call shape ------------------------------------------

export type CallDeps = {
  model: string;
  env: { OPENROUTER_API_KEY: string } & Record<string, string>;
  client?: unknown;
  callModel?: (req: unknown, opts: unknown) => Promise<ModelResult>;
  tools?: unknown[];
  system?: string;
  stepCap?: number;
  AbortControllerCtor?: typeof AbortController;
  timeoutMs?: number;
  log?: (stage: string, msg: string, extra?: Record<string, unknown>) => void;
  errlog?: (stage: string, msg: string, extra?: Record<string, unknown>) => void;
};

export type ModelResult = {
  getText: () => Promise<string>;
  getResponse: () => Promise<ChatResult>;
  getToolCalls?: () => Promise<unknown[]>;
};

export type ChatResult = {
  id?: string;
  model?: string;
  text?: string;
  output?: unknown[];
  choices?: unknown[];
  usage?: UsageLike;
};

export type UsageLike = {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  cost?: number;
  inputTokens?: number;
  outputTokens?: number;
  promptTokens?: number;
  completionTokens?: number;
  totalTokens?: number;
  inputTokensDetails?: { cachedTokens?: number; cacheWriteTokens?: number };
  promptTokensDetails?: { cachedTokens?: number; cache_write_tokens?: number };
  outputTokensDetails?: { reasoningTokens?: number };
  completionTokensDetails?: { reasoningTokens?: number };
  costDetails?: Record<string, number>;
  cost_details?: Record<string, number>;
  isByok?: boolean;
  is_byok?: boolean;
  serverToolUseDetails?: Record<string, unknown>;
  server_tool_use_details?: Record<string, unknown>;
  cached_tokens?: number;
  cache_write_tokens?: number;
  reasoning_tokens?: number;
  cost_prompt_usd?: number;
  cost_completion_usd?: number;
  cost_upstream_usd?: number;
  request_id?: string;
  [key: string]: unknown;
};

export type CallResult = {
  text: string;
  usage: UsageLike;
  model: string;
  requestId?: string;
  durationMs: number;
  stepCount: number;
};

// --- OpenRouter orchestrator result -------------------------------------

export type RunOpenCodeSkillFn = (
  openrouterApiKey: string,
  ctx: Ctx,
  deps: Deps,
) => Promise<Review>;

// --- Octokit / GitHub shapes --------------------------------------------

export type OctokitLike = {
  rest: {
    issues: {
      getComment: (args: unknown) => Promise<{ data: { body: string; id: number } }>;
      createComment: (args: unknown) => Promise<{ data: { id: number; body: string } }>;
      updateComment: (args: unknown) => Promise<{ data: unknown }>;
      listComments: (args: unknown) => Promise<{ data: Array<{ id: number; body: string; user?: { login?: string }; node_id?: string }> }>;
    };
    pulls: {
      createReviewComment: (args: unknown) => Promise<{ data: unknown }>;
      listReviewComments: (args: unknown) => Promise<{ data: Array<{ id: number; body: string }> }>;
    };
    reactions: {
      createForIssueComment: (args: unknown) => Promise<{ data: unknown }>;
    };
  };
};

export type OctokitCtorLike = new (args: { auth: string }) => OctokitLike;

export type JwtLike = {
  sign: (
    payload: Record<string, unknown>,
    secret: string,
    options: Record<string, unknown>,
  ) => string;
};

export type FetchLike = (
  url: string,
  opts?: Record<string, unknown>,
) => Promise<{
  ok: boolean;
  status: number;
  text: () => Promise<string>;
  json: () => Promise<unknown>;
}>;

export type FsLike = {
  readFile: (path: string, encoding?: string) => Promise<string | Buffer>;
  writeFile: (path: string, body: string, opts?: { mode?: number }) => Promise<void>;
  unlink: (path: string) => Promise<void>;
  rm: (path: string, opts?: { recursive?: boolean; force?: boolean }) => Promise<void>;
  mkdir: (path: string, opts?: { recursive?: boolean }) => Promise<void>;
};

export type ExecFileLike = (
  cmd: string,
  args: string[],
  opts?: Record<string, unknown>,
) => Promise<{ stdout: string; stderr?: string; exitCode?: number }>;

export type SpawnFn = (
  cmd: string,
  args: string[],
  opts?: Record<string, unknown>,
) => unknown;

export type RtkAdapter = {
  readFile: (path: string, encoding: string, options?: Record<string, unknown>) => Promise<string>;
  init: () => Promise<{ source: string; binary: string | null; reason: string | null }>;
  source: string;
};

// --- Cleanup registry ---------------------------------------------------

export type CleanupRegistry = {
  register: (fn: () => Promise<void> | void) => void;
  runAll: () => Promise<void>;
};

// --- Generic error stamp (sdk.ts sets these on the thrown Error) -------

export type SdkErrorShape = {
  statusCode?: number;
  errorContentType?: string;
  errorBody?: string;
  durationMs?: number;
  raw?: string;
  stackDetail?: string;
};
