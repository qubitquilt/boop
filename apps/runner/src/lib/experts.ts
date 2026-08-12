// Multi-expert review sub-workflow.
//
// QUB-95: the dispatch sub-stage picks a set of experts
// based on the classify sub-stage's output (QUB-94), runs
// them in parallel as independent LLM invocations, then
// gathers the findings. The narrate sub-stage produces the
// cohesive summary + inline-comment set from the gathered
// findings.
//
// Each expert is an independent OpenRouter SDK call. The
// system prompt is the lens file (one of eight
// `agents/review-*.md`). The user message is the
// walkthrough + the diff + the PR context. The expert
// returns a JSON object with a `findings` array; the
// orchestrator in workflow.ts (gather / meta-review /
// narrate) consumes that shape.
//
// The expert model:
//   - Each expert is an async function (ctx, deps, shared)
//     that returns { findings: Finding[] }.
//   - Findings are simple records: { id, expert, severity,
//     title, body, path?, line? }.
//   - The orchestrator (pickExperts) maps a PR type to a
//     set of expert names; the runner resolves each name to
//     an expert function via EXPERT_POOL.
//
// Per-expert failure handling lives in the dispatch stage:
// runExperts awaits all experts in parallel. A single
// failure rejects the whole dispatch, and the workflow's
// retry machinery re-attempts the dispatch. The experts
// themselves are deterministic per call (no shared state
// across retries), so re-attempts are safe.

import {
  callOpenRouter,
  emptyTelemetry,
  stripOpenRouterPrefix,
  readWithRetry,
} from "./openrouter.ts";
import { safeJsonParse } from "./json_shape.ts";
import { stripFrontmatter } from "./prompt_parts.ts";
import { buildAgentTools, toolsAvailable } from "./tools.ts";
import {
  EXPERTS,
  LENS_TO_EXPERT,
  EXPERT_TO_LENS,
} from "./experts/registry.ts";
import type {
  Ctx,
  Classification,
  Deps,
  ExpertFn,
  ExpertResult,
  ExpertShared,
  Finding,
  MetaReviewFn,
  LensTelemetry,
  Review,
} from "../types.ts";

export function pickExperts(classification: Classification | null | undefined): string[] {
  const type = (classification && classification.type) || "unknown";
  switch (type) {
    case "bug-fix":
      return ["regression-hunter", "test-quality"];
    case "feature":
      return ["api-design", "error-handling", "test-quality"];
    case "refactor":
      return ["design-pattern", "readability"];
    case "docs":
      return ["readability"];
    case "test-only":
      return ["test-quality"];
    case "infra":
      return ["regression-hunter", "design-pattern"];
    case "unknown":
    default:
      return ["design-pattern", "readability"];
  }
}

export const EXPERT_POOL: Record<string, ExpertFn> = Object.fromEntries(
  Object.keys(EXPERTS).map((name) => [name, ((ctx: Ctx, deps: Deps, shared?: ExpertShared) => defaultExpert(name, ctx, deps, shared)) as ExpertFn]),
);

function buildExpertPrompt(name: string, ctx: Ctx, deps: Deps, walkthrough: string): string {
  const wt = walkthrough || "(walkthrough unavailable — read the diff directly)";
  const toolsOn = toolsAvailable(ctx, deps);
  return [
    "# Task",
    "",
    `You are the **${name}** expert on a multi-lens PR review.`,
    "Apply your lens checklist to the change below. Report what you",
    "find; do not re-state what the PR does. The walkthrough is for",
    "orientation; the diff is the ground truth.",
    "",
    "# Walkthrough (human-readable summary of the change)",
    "",
    wt,
    "",
    "# Diff range",
    "",
    "```",
    `pr_owner: ${ctx.prOwner}`,
    `pr_repo: ${ctx.prRepo}`,
    `pr_number: ${ctx.prNumber}`,
    `pr_head_sha: ${ctx.prHeadSha}`,
    `pr_base_ref: ${ctx.prBaseRef}`,
    `working_directory: ${ctx.paths?.repoDir || "/work/repo"}`,
    "```",
    "",
    "Read the diff. Apply your lens checklist. Report findings as JSON.",
    ...(toolsOn
      ? [
          "",
          "# Tools available for verification",
          "",
          "You have a small agent tool set for verification: `run_command` " +
            "(run a shell command in the PR's working directory with a " +
            "timeout + output cap — useful for running the PR's test suite), " +
            "`read_file` (read a file inside the repo), and `git_diff` " +
            "(run `git diff <range>` for a path). The tool guard rejects " +
            "network primitives and references to the runner's secret mounts. " +
            "Use these tools to verify a finding (e.g. confirm a test failure, " +
            "ground a line number) before reporting it; do NOT emit raw tool-" +
            "call JSON in your final response — the SDK runs tools natively " +
            "and your final text must be the JSON findings object below.",
        ]
      : []),
    "",
    "# Output spec",
    "",
    "Return a single JSON object:",
    "",
    "```json",
    "{",
    '  "findings": [',
    "    {",
    '      "id": "unique-id-for-this-finding",',
    '      "severity": "blocking | follow-up | optional | info",',
    '      "title": "one-line summary",',
    '      "body": "1-3 sentence prose. No Observation/Impact/Suggestion formula.",',
    '      "path": "path/to/file.ext",  // optional; omit for cross-cutting findings',
    '      "line": 42  // optional; line in the post-diff file',
    "    }",
    "  ]",
    "}",
    "```",
    "",
    "Rules:",
    "- One finding per bullet. Do not combine unrelated concerns.",
    "- Cite exact file paths and line numbers when you have them.",
    "- Use severity = blocking only for things that would survive",
    "  an honest 'I disagree' from the author.",
    "- Return an empty `findings: []` array when the lens has nothing",
    "  to flag. An empty result is a successful review.",
    "- Do not include a preamble. The JSON is the entire response.",
  ].join("\n");
}

async function defaultExpert(
  name: string,
  ctx: Ctx,
  deps: Deps,
  shared: ExpertShared = {},
): Promise<ExpertResult> {
  const lensFile = EXPERT_TO_LENS[name];
  if (!lensFile) {
    throw new Error(
      `expert "${name}" has no lens file mapped; update EXPERTS in lib/experts/registry.ts`,
    );
  }
  const configSrc = deps?.paths?.configSrc || "/home/opencode/.config/opencode";
  const lensPath = `${configSrc}/skills/boop/agents/${lensFile}.md`;
  const override = deps.expertOverrides?.[name];
  if (typeof override === "function") {
    return override(ctx, deps, shared);
  }
  const fs = deps.fs;
  const lensBody = await readLensBody(lensPath, fs, deps);
  const walkthrough = shared.walkthrough || ctx.walkthrough || "";
  const userPrompt = buildExpertPrompt(name, ctx, deps, walkthrough);
  const EXPERT_TIMEOUT_MS = 300_000;
  const callOpenRouterFn = deps.callOpenRouter || callOpenRouter;
  const expertTools = buildAgentTools(ctx, deps);
  let callResult;
  try {
    callResult = await callOpenRouterFn(userPrompt, {
      ...deps,
      model: stripOpenRouterPrefix(ctx.openrouterModel),
      timeoutMs: EXPERT_TIMEOUT_MS,
      system: lensBody,
      tools: expertTools,
    });
  } catch (err) {
    if (err && typeof err === "object") (err as { expert?: string }).expert = name;
    throw err;
  }
  const parsed = parseExpertResponse(callResult?.text, name);
  for (const f of parsed.findings) {
    if (f && !f.expert) f.expert = name;
  }
  const u = callResult?.usage || {};
  const telemetry: LensTelemetry | null = callResult
    ? {
        lens: name,
        model: callResult.model || ctx.openrouterModel || "",
        provider: "openrouter",
        inputTokens: (u["prompt_tokens"] as number) ?? 0,
        outputTokens: (u["completion_tokens"] as number) ?? 0,
        reasoningTokens: (u["reasoning_tokens"] as number) ?? 0,
        cacheReadTokens: (u["cached_tokens"] as number) ?? 0,
        cacheWriteTokens: 0,
        costUsd: (u["cost"] as number) ?? 0,
        stepCount:
          typeof callResult.stepCount === "number" && callResult.stepCount > 0
            ? callResult.stepCount
            : 1,
      }
    : null;
  return { findings: parsed.findings, telemetry };
}

async function readLensBody(lensPath: string, fs: Deps["fs"], deps: Deps): Promise<string> {
  const reader: (p: string) => Promise<string> = deps.rtk && typeof deps.rtk.readFile === "function"
    ? async (p: string) => deps.rtk!.readFile(p, "utf8")
    : async (p: string) => {
        const data = await fs.readFile(p, "utf8");
        return typeof data === "string" ? data : data.toString("utf8");
      };
  const attempts = deps.retries?.lens ?? 5;
  const body = await readWithRetry(lensPath, reader, {
    attempts,
    onRetry: (n, err) =>
      deps.log?.("lens", `read attempt ${n} failed`, {
        err: String(err instanceof Error ? err.message : err),
      }),
  });
  return stripFrontmatter(body || "");
}

type ParsedExpert = { findings: Finding[] };

function parseExpertResponse(text: string | null | undefined, name: string): ParsedExpert {
  if (!text || !text.trim()) {
    return { findings: [] };
  }
  const parsed = safeJsonParse<{ findings?: unknown } | null>(text, null);
  if (!parsed || typeof parsed !== "object") {
    return {
      findings: [
        {
          id: `${name}-unparsed-${Date.now()}`,
          expert: name,
          severity: "info",
          title: `${name} (unparsed response)`,
          body: (text || "").trim().slice(0, 2000),
        },
      ],
    };
  }
  const findings = Array.isArray(parsed.findings) ? parsed.findings : [];
  const out: Finding[] = [];
  for (let i = 0; i < findings.length; i++) {
    const f = findings[i] as Partial<Finding> | null;
    if (!f) continue;
    const f_id = typeof f.id === "string" && f.id ? f.id : `${name}-${i}`;
    out.push({
      id: f_id,
      expert: name,
      severity: normalizeSeverity(f.severity),
      title: typeof f.title === "string" ? f.title : "(no title)",
      body: typeof f.body === "string" ? f.body : "",
      ...(typeof f.path === "string" ? { path: f.path } : {}),
      ...(Number.isInteger(f.line) ? { line: f.line as number } : {}),
    });
  }
  return { findings: out };
}

function normalizeSeverity(s: unknown): Finding["severity"] {
  switch (s) {
    case "blocking":
    case "follow-up":
    case "optional":
    case "info":
      return s;
    default:
      return "info";
  }
}

export async function runExperts(
  names: string[],
  ctx: Ctx,
  deps: Deps,
  shared: ExpertShared = {},
): Promise<{ findings: Finding[]; lensTelemetry: LensTelemetry[] }> {
  if (!Array.isArray(names) || names.length === 0) {
    return { findings: [], lensTelemetry: [] };
  }
  const tasks = names.map((name) => {
    const fn = deps.expertOverrides?.[name] || EXPERT_POOL[name];
    if (!fn) {
      throw new Error(`unknown expert: ${name}`);
    }
    return Promise.resolve()
      .then(() => fn(ctx, deps, shared))
      .then((result) => ({ name, ok: true as const, result }))
      .catch((err) => ({ name, ok: false as const, err }));
  });
  const results = await Promise.all(tasks);
  const failed = results.filter((r) => !r.ok);
  if (failed.length > 0) {
    const failedNames = failed.map((f) => f.name).join(", ");
    const first = failed[0]?.err;
    const wrapped: Error & { failed?: Array<{ name: string; err: unknown }> } = new Error(
      `expert dispatch failed for: ${failedNames} (first: ${String(first instanceof Error ? first.message : first)})`,
    );
    wrapped.failed = failed.map((f) => ({ name: f.name, err: f.err }));
    throw wrapped;
  }
  const findings: Finding[] = [];
  const lensTelemetry: LensTelemetry[] = [];
  for (const r of results) {
    if (!r || !r.ok || !r.result) continue;
    const res = r.result as ExpertResult | Finding[];
    const f = Array.isArray(res)
      ? res
      : Array.isArray((res as ExpertResult).findings)
        ? (res as ExpertResult).findings
        : [];
    if (f.length > 0) findings.push(...f);
    if (!Array.isArray(res) && (res as ExpertResult).telemetry) {
      const t = (res as ExpertResult).telemetry as Partial<LensTelemetry> | null;
      if (t) {
        lensTelemetry.push({
          lens: r.name,
          model: t.model ?? "",
          provider: t.provider ?? "",
          inputTokens: t.inputTokens ?? 0,
          outputTokens: t.outputTokens ?? 0,
          reasoningTokens: t.reasoningTokens ?? 0,
          cacheReadTokens: t.cacheReadTokens ?? 0,
          cacheWriteTokens: t.cacheWriteTokens ?? 0,
          costUsd: t.costUsd ?? 0,
          stepCount: t.stepCount ?? 1,
        });
      }
    }
  }
  return { findings, lensTelemetry };
}

export const defaultMetaReview: MetaReviewFn = async (
  _findings,
  _classification,
  _ctx,
  _deps,
) => {
  return { reDispatch: [] };
};

export function mergeByExpert(
  original: Finding[],
  replacement: Finding[],
  droppedExperts: Set<string> | null = null,
): Finding[] {
  const reDispatchedExperts =
    droppedExperts ||
    new Set((replacement || []).map((f) => f.expert).filter(Boolean) as string[]);
  const kept = (original || []).filter(
    (f) => !f || !f.expert || !reDispatchedExperts.has(f.expert),
  );
  const all = [...kept, ...(replacement || [])];
  return gather(all);
}

export function gather(findings: Finding[] | null | undefined): Finding[] {
  const seen = new Set<string>();
  const out: Finding[] = [];
  for (const f of findings || []) {
    if (!f || !f.id) continue;
    if (seen.has(f.id)) continue;
    seen.add(f.id);
    out.push(f);
  }
  return out;
}

export async function defaultNarrate(
  findings: Finding[],
  _ctx: Ctx,
  _deps: Deps,
): Promise<Review> {
  const expertNames = [...new Set((findings || []).map((f) => f.expert))];
  const summary = expertNames.length
    ? `## TL;DR\n\nBoop's multi-expert review consulted ${expertNames.join(", ")}.\n\n_Stub narrative; the real narrator LLM call lands in a follow-up._`
    : "## TL;DR\n\nNo expert findings to narrate.";
  const inlineComments = (findings || [])
    .filter((f) => f && f.path && Number.isInteger(f.line))
    .map((f) => ({
      path: f.path as string,
      line: f.line as number,
      body: `[${f.expert}] ${f.body}`,
    }));
  return {
    summary,
    inlineComments,
    confidence: expertNames.length > 0 ? "medium" : "low",
    parseError: null,
    telemetry: null,
  };
}

const PLACEHOLDER_NARRATE_SUMMARY = [
  "## TL;DR",
  "",
  "No blocking issues found. The diff is small and well-scoped; the multi-expert review did not flag anything actionable.",
  "",
  "## Findings",
  "",
  "| Severity | Location | Issue |",
  "|----------|----------|-------|",
  "| — | — | No issues flagged. |",
  "",
  "## What this PR does well",
  "",
  "- The change is focused and minimal.",
  "- The code follows the existing project conventions.",
].join("\n");

export function placeholderNarrate(
  findings: Finding[],
  walkthrough: string,
  walkthroughIsPlaceholder: boolean,
  _ctx: Ctx,
  _deps: Deps,
): Review {
  return {
    summary: PLACEHOLDER_NARRATE_SUMMARY,
    inlineComments: [],
    confidence: "high",
    parseError: null,
    telemetry: {
      ...emptyTelemetry(),
      stepCount: 0,
    },
  };
}

export const _PLACEHOLDER_NARRATE_SUMMARY = PLACEHOLDER_NARRATE_SUMMARY;
