// Output parser.
//
// Extracts the structured SUMMARY / INLINE COMMENTS / CONFIDENCE /
// END block from the narrator LLM's text. The "looks like review
// shape" sanity check rejects the observed non-review shapes
// (shell transcripts, raw error strings, tool-call hallucinations)
// so the runner refuses to post garbage instead of polluting the PR.

/**
 * parseReviewOutput extracts the structured SUMMARY / INLINE
 * COMMENTS / CONFIDENCE / END block from the assistant text.
 * Anything before "=== SUMMARY ===" is dropped. The INLINE
 * COMMENTS section is parsed as one "path:line: body" per line.
 * The optional CONFIDENCE section is parsed as `high`, `medium`,
 * or `low`; missing or unrecognized values default to `medium` so
 * older models keep working.
 *
 * Failure modes (no structured block, or a structured block whose
 * body fails the structure sanity check) return
 * { summary: "", confidence: "low", parseError: "<reason>" }. The
 * caller MUST check `!result.summary` and skip the post. Returning
 * a non-empty summary in either failure mode is what allowed the
 * 2026-08-03 "garbage on the PR" regression (PR #90 / #92).
 */
export function parseReviewOutput(output) {
  const summaryMatch = output.match(
    /===\s*SUMMARY\s*===\s*([\s\S]*?)\s*===[\s\S]*?INLINE COMMENTS\s*===\s*([\s\S]*?)\s*===\s*(?:CONFIDENCE\s*===\s*([\s\S]*?)\s*===\s*)?END\s*===/i,
  );
  if (!summaryMatch) {
    return {
      summary: "",
      inlineComments: [],
      confidence: "low",
      parseError: "no structured block",
    };
  }

  const summary = summaryMatch[1].trim();
  const inlineBlock = summaryMatch[2].trim();
  const confidenceRaw = (summaryMatch[3] || "").trim().toLowerCase();

  const shape = looksLikeReviewShape(summary);
  if (!shape.ok) {
    return {
      summary: "",
      inlineComments: [],
      confidence: "low",
      parseError: shape.reason,
    };
  }

  const inlineComments = [];
  for (const rawLine of inlineBlock.split("\n")) {
    const line = rawLine.trim();
    if (!line) continue;
    // Match "<path>:<line>: <body>" where path may contain slashes
    // and dots, line is a positive integer, and body is the rest.
    const m = line.match(/^(\S+?):(\d+):\s+(.*)$/);
    if (!m) continue;
    const [, refPath, lineStr, body] = m;
    const lineNum = Number(lineStr);
    if (!Number.isInteger(lineNum) || lineNum < 1) continue;
    inlineComments.push({ path: refPath, line: lineNum, body });
  }

  const confidence = ["high", "medium", "low"].includes(confidenceRaw)
    ? confidenceRaw
    : "medium";

  return { summary, inlineComments, confidence, parseError: null };
}

// looksLikeReviewShape is the structure sanity check applied to the
// SUMMARY body before the runner posts it. The LLM sometimes echoes
// patterns from the diff (a test fixture, a fake shell transcript,
// an error string, the build header) and the parser happily matches
// a `=== SUMMARY ===` wrapper around the echo. The shape check
// rejects the obvious garbage patterns so the runner can refuse to
// post instead of polluting the PR.
//
// A real review summary is at least 200 bytes (a short TL;DR plus a
// findings table is comfortably above this), contains a markdown
// heading or finding table, and does not look like source code.
function looksLikeReviewShape(s) {
  if (!s) {
    return { ok: false, reason: "summary empty" };
  }
  // Pattern checks first: when the body is one of the observed
  // non-review outputs, surface the specific reason even if the
  // body is short.
  // JS string-concat echo: the LLM mirrors a test file's `"...\n" +`
  // concatenation pattern. Two common giveaways.
  if (/\\n"\s*\+\s*\n/.test(s) || /^\s*\+[ \t]+"/m.test(s)) {
    return { ok: false, reason: "JS string-concat echo" };
  }
  // Non-review outputs the LLM has been observed to emit as the
  // "summary" body: fake shell transcripts, raw error strings, and
  // the model build header. The `&& !/^##/m.test(s)` guard lets
  // a real review that *mentions* `$ git status` in its prose pass.
  if (/^\s*\$ git /m.test(s) && !/^##/m.test(s)) {
    return { ok: false, reason: "shell transcript (no markdown heading)" };
  }
  if (/^\s*Error: /m.test(s) && !/^##/m.test(s)) {
    return { ok: false, reason: "raw error string (no markdown heading)" };
  }
  if (/^>\s*build\s*·/m.test(s) && !/^##/m.test(s)) {
    return { ok: false, reason: "build header (no markdown heading)" };
  }
  // Tool-call hallucination. The narrator LLM sometimes emits a
  // tool invocation (opencode / Claude / OpenAI / Anthropic shapes)
  // as its "summary" body, either as bare JSON or wrapped in
  // <tool_use> / <tool_call> / [TOOL_CALL] blocks. The narrator is
  // a single chat completion with no tools enabled, so any tool
  // call is a hallucination. The "no structured block" parse
  // path already catches the bare-JSON case when it has no
  // `=== SUMMARY ===` marker, but the LLM can also wrap the
  // hallucination inside the markers and pass the regex while
  // still not being a review. Match both the wrapped and the
  // bare forms so the runner fails loud instead of posting a
  // tool call to the PR.
  //
  // The bare-JSON patterns are matched at the "name"/"arguments"
  // string level (not balanced-brace level) so the regex stays
  // simple and the nested `{}` in the arguments value does not
  // trip it. The shapes pinned here mirror the four most common
  // tool-call serialization formats observed across the model
  // family.
  if (
    // opencode / Claude: "name": "tool_id", "arguments": { ... }
    /"name"\s*:\s*"[a-z_][a-z0-9_]*"\s*,\s*"arguments"\s*:/i.test(s) ||
    // OpenAI: "function": { "name": "tool_id", "arguments": { ... } }
    /"function"\s*:\s*\{\s*"name"\s*:\s*"[a-z_][a-z0-9_]*"\s*,\s*"arguments"/i.test(s) ||
    // Anthropic <tool_use>...</tool_use>
    /<tool[_-]?use[\s>]/i.test(s) ||
    // opencode <tool_call>...</tool_call> XML
    /<\/?tool[_-]?call>/i.test(s) ||
    // Bracket wrapper
    /\[TOOL_CALL\]/i.test(s)
  ) {
    return { ok: false, reason: "tool-call hallucination" };
  }
  // Length sanity check. A real review is at least 200 bytes —
  // a short TL;DR plus a one-row finding table is comfortably above
  // this. The 200-byte floor catches the case where the LLM emits
  // a tiny stub that happens to contain a heading but no real content.
  if (s.length < 200) {
    return { ok: false, reason: "summary too short (< 200 bytes)" };
  }
  // Must contain at least one of the standard review sections or a
  // finding table. Real reviews always have one of these markers;
  // the LLM that produces prose without them is probably faking it.
  const hasHeading = /^##\s+(TL;DR|Findings|What this PR does well|Non-Issues)/m.test(s);
  const hasTable = /^\|.+\|.+\|/m.test(s);
  if (!hasHeading && !hasTable) {
    return { ok: false, reason: "no markdown heading or finding table" };
  }
  return { ok: true };
}
