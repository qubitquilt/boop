// Output parser.
//
// Extracts the structured SUMMARY / INLINE COMMENTS / CONFIDENCE /
// END block from the narrator LLM's text. The "looks like review
// shape" sanity check rejects the observed non-review shapes
// (shell transcripts, raw error strings, tool-call hallucinations)
// so the runner refuses to post garbage instead of polluting the PR.

import type { Review } from "../../types.ts";

/**
 * parseReviewOutput extracts the structured SUMMARY / INLINE
 * COMMENTS / CONFIDENCE / END block from the assistant text.
 * Anything before "=== SUMMARY ===" is dropped. The INLINE
 * COMMENTS section is parsed as one "path:line: body" per line.
 * The optional CONFIDENCE section is parsed as `high`, `medium`,
 * or `low`; missing or unrecognized values default to `medium` so
 * older models keep working.
 */
export function parseReviewOutput(output: string): Review {
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

  const summary = (summaryMatch[1] || "").trim();
  const inlineBlock = (summaryMatch[2] || "").trim();
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

  const inlineComments: Review["inlineComments"] = [];
  for (const rawLine of inlineBlock.split("\n")) {
    const line = rawLine.trim();
    if (!line) continue;
    const m = line.match(/^(\S+?):(\d+):\s+(.*)$/);
    if (!m) continue;
    const refPath = m[1];
    const lineStr = m[2];
    const body = m[3];
    if (!refPath || !lineStr || body == null) continue;
    const lineNum = Number(lineStr);
    if (!Number.isInteger(lineNum) || lineNum < 1) continue;
    inlineComments.push({ path: refPath, line: lineNum, body });
  }

  const confidence: Review["confidence"] = ["high", "medium", "low"].includes(
    confidenceRaw,
  )
    ? (confidenceRaw as Review["confidence"])
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
function looksLikeReviewShape(s: string): { ok: true } | { ok: false; reason: string } {
  if (!s) {
    return { ok: false, reason: "summary empty" };
  }
  if (/\\n"\s*\+\s*\n/.test(s) || /^\s*\+[ \t]+"/m.test(s)) {
    return { ok: false, reason: "JS string-concat echo" };
  }
  if (/^\s*\$ git /m.test(s) && !/^##/m.test(s)) {
    return { ok: false, reason: "shell transcript (no markdown heading)" };
  }
  if (/^\s*Error: /m.test(s) && !/^##/m.test(s)) {
    return { ok: false, reason: "raw error string (no markdown heading)" };
  }
  if (/^>\s*build\s*·/m.test(s) && !/^##/m.test(s)) {
    return { ok: false, reason: "build header (no markdown heading)" };
  }
  if (
    /"name"\s*:\s*"[a-z_][a-z0-9_]*"\s*,\s*"arguments"\s*:/i.test(s) ||
    /"function"\s*:\s*\{\s*"name"\s*:\s*"[a-z_][a-z0-9_]*"\s*,\s*"arguments"/i.test(s) ||
    /<tool[_-]?use[\s>]/i.test(s) ||
    /<\/?tool[_-]?call>/i.test(s) ||
    /\[TOOL_CALL\]/i.test(s)
  ) {
    return { ok: false, reason: "tool-call hallucination" };
  }
  if (s.length < 200) {
    return { ok: false, reason: "summary too short (< 200 bytes)" };
  }
  const hasHeading = /^##\s+(TL;DR|Findings|What this PR does well|Non-Issues)/m.test(s);
  const hasTable = /^\|.+\|.+\|/m.test(s);
  if (!hasHeading && !hasTable) {
    return { ok: false, reason: "no markdown heading or finding table" };
  }
  return { ok: true };
}
