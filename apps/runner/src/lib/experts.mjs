// Multi-expert review sub-workflow.
//
// QUB-95: the dispatch sub-stage picks a set of experts
// based on the classify sub-stage's output (QUB-94), runs
// them in parallel as independent LLM invocations, then
// gathers the findings. The narrate sub-stage produces the
// cohesive summary + inline-comment set from the gathered
// findings (replacing the single-LLM-call path of
// runOpenCodeSkill).
//
// Today every expert is a stub: each returns a placeholder
// finding. The real LLM call (an OpenRouter SDK chat
// completion per expert with a tailored prompt + tool
// surface) is a follow-up. The override hook lets a test
// inject specific experts.
//
// The expert model:
//   - Each expert is an async function (ctx, deps, shared)
//     that returns { findings: Finding[] }.
//   - Findings are simple records: { id, expert, severity,
//     title, body }.
//   - The orchestrator (pickExperts) maps a PR type to a
//     set of expert names; the runner resolves each name to
//     an expert function via EXPERT_POOL.

// pickExperts is the orchestrator. Maps a PR type to a
// list of expert names. The default mapping is a starting
// point; a future PR can tune it based on real PR traffic.
//
// Expert names are stable strings (the dashboard surfaces
// them). Adding a name means adding an entry to EXPERT_POOL.
export function pickExperts(classification) {
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
      // Conservative default: the two most-general experts.
      // QUB-96's meta-review will catch anything the
      // default pool misses on a re-pass.
      return ["design-pattern", "readability"];
  }
}

// EXPERT_POOL is the registry of expert functions. Each
// function is an async (ctx, deps) -> { findings }. The
// default implementations are stubs; the real LLM
// invocations land in a follow-up PR.
export const EXPERT_POOL = {
  "regression-hunter": defaultExpert.bind(null, "regression-hunter"),
  "test-quality": defaultExpert.bind(null, "test-quality"),
  "api-design": defaultExpert.bind(null, "api-design"),
  "error-handling": defaultExpert.bind(null, "error-handling"),
  "design-pattern": defaultExpert.bind(null, "design-pattern"),
  "readability": defaultExpert.bind(null, "readability"),
};

// defaultExpert is the stub. Returns a single finding with
// the expert's name + a placeholder body. The real expert
// will run an OpenRouter SDK chat completion with a
// tailored prompt + the PR diff + tool access (bash, file
// reads, test runner) scoped to the cloned repo.
async function defaultExpert(name, _ctx, _deps) {
  return {
    findings: [
      {
        id: `${name}-placeholder`,
        expert: name,
        severity: "info",
        title: `${name} (stub)`,
        body: `Stub finding from ${name}. A follow-up PR wires the real LLM call.`,
      },
    ],
  };
}

// runExperts runs a list of expert names in parallel and
// returns the concatenated findings. Each expert runs
// independently — a single expert failure does not block
// the others. Promise.allSettled would be more permissive
// (one failure doesn't fail the whole dispatch); the
// current implementation uses Promise.all so an expert
// failure propagates as a hard error (caught by the gate
// + retry machinery in workflow.mjs).
//
// The shared object lets the experts coordinate (e.g., an
// expert can see findings already produced by an earlier
// expert in the same dispatch). For the stub, shared is
// unused. The real experts will read shared.findings to
// avoid duplicate work.
export async function runExperts(names, ctx, deps, shared = {}) {
  const tasks = names.map((name) => {
    const fn = EXPERT_POOL[name];
    if (!fn) {
      throw new Error(`unknown expert: ${name}`);
    }
    return fn(ctx, deps, shared);
  });
  const results = await Promise.all(tasks);
  const findings = [];
  for (const r of results) {
    if (r && Array.isArray(r.findings)) {
      findings.push(...r.findings);
    }
  }
  return findings;
}

// defaultMetaReview is the stub. It accepts every finding
// and requests no re-pass. A follow-up PR wires the real
// meta-reviewer LLM call: an OpenRouter SDK chat completion
// that scans the gathered findings for things that "stick
// out as potentially wrong" (false positives,
// contradictions, missing context) and returns the list of
// experts to re-dispatch.
//
// The return shape is { reDispatch: string[] } — the names
// of the experts whose findings need a re-pass. An empty
// list means "all findings are fine; narrate them as-is."
export async function defaultMetaReview(_findings, _classification, _ctx, _deps) {
  return { reDispatch: [] };
}

// mergeByExpert replaces the findings from the re-dispatched
// experts with the new findings. Other experts' findings
// are preserved. The merge keeps the result flat (no
// nesting) and de-duped by id.
//
// droppedExperts is the explicit list of expert names to
// drop from the original. The meta-reviewer signals a
// re-pass for these experts; ALL old findings from these
// experts are removed, even if the re-pass returned no new
// findings for them (an empty re-pass is a valid rejection).
// When droppedExperts is not provided, the function falls
// back to the previous behavior: drop the experts that
// appear in the replacement set.
export function mergeByExpert(original, replacement, droppedExperts = null) {
  const reDispatchedExperts =
    droppedExperts ||
    new Set((replacement || []).map((f) => f.expert).filter(Boolean));
  const kept = (original || []).filter(
    (f) => !f || !f.expert || !reDispatchedExperts.has(f.expert),
  );
  const all = [...kept, ...(replacement || [])];
  return gather(all);
}

// gather flattens expert findings into a unified, de-duped
// list. Two findings are "the same" if they share the same
// id (the expert names the id; collisions indicate a
// duplicate finding the dispatcher should not post twice).
// Today the default experts emit one finding each with a
// unique id, so the dedupe is a no-op. The real experts
// will occasionally collide; the dedupe keeps the inlines
// list clean.
export function gather(findings) {
  const seen = new Set();
  const out = [];
  for (const f of findings || []) {
    if (!f || !f.id) continue;
    if (seen.has(f.id)) continue;
    seen.add(f.id);
    out.push(f);
  }
  return out;
}

// narrate synthesizes the cohesive summary + inlines from
// the gathered findings. The default returns a placeholder
// review whose body mentions the expert names; a follow-up
// PR wires the real narrator LLM call.
//
// The shape mirrors the existing runOpenCodeSkill output
// so the downstream summary / inlines stages are
// unaffected: { summary, inlineComments, confidence,
// telemetry }.
export async function defaultNarrate(findings, _ctx, _deps) {
  const expertNames = [...new Set((findings || []).map((f) => f.expert))];
  const summary = expertNames.length
    ? `## TL;DR\n\nBoop's multi-expert review consulted ${expertNames.join(", ")}.\n\n_Stub narrative; the real narrator LLM call lands in a follow-up._`
    : "## TL;DR\n\nNo expert findings to narrate.";
  const inlineComments = (findings || [])
    .filter((f) => f && f.path && Number.isInteger(f.line))
    .map((f) => ({
      path: f.path,
      line: f.line,
      body: `[${f.expert}] ${f.body}`,
    }));
  return {
    summary,
    inlineComments,
    confidence: expertNames.length > 0 ? "medium" : "low",
    telemetry: null,
  };
}
