// JSON shape helpers.
//
// Shared JSON-fence strip and parse-or-default used by every
// LLM response parser in the runner. The "looks like JSON"
// parsers (parseExpertResponse in experts.mjs, parseReviewOutput
// in openrouter/parser.mjs) all share the same JSON-fence shape;
// a future Claude 4.5 ` ```jsonc ` fence is one edit instead of
// one edit per call site.

/**
 * Strip a leading ` ```json ` (or ` ``` `) fence and a trailing
 * ` ``` ` from a string, returning the trimmed result. The LLM
 * is asked to return raw JSON but sometimes wraps it in a code
 * block; this helper normalises the wrap. The fence tags are
 * matched case-insensitively so ` ```JSON ` is also stripped.
 */
export function stripJsonFence(text) {
  if (!text) return "";
  return String(text)
    .replace(/^```(?:json)?\s*/i, "")
    .replace(/```\s*$/, "")
    .trim();
}

/**
 * Parse the JSON in `text`, returning `defaultValue` on any
 * failure. `text` is fence-stripped first via `stripJsonFence`.
 * Callers use this to default to a benign shape (e.g.
 * `{ findings: [] }`) when the LLM returns a non-JSON response.
 */
export function safeJsonParse(text, defaultValue) {
  const stripped = stripJsonFence(text);
  if (!stripped) return defaultValue;
  try {
    return JSON.parse(stripped);
  } catch {
    return defaultValue;
  }
}
