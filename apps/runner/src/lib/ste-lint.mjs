// STE (Simplified Technical English) linter.
//
// Runs the mechanical STE rules against the LLM's review
// output before the runner posts it. The linter is a sanity
// check, not a full STE validator — it catches the rules
// the narrator can drift on (contractions, semicolons,
// marketing adjectives, long sentences) and surfaces them
// in the run log. It does not modify the output. The LLM is
// the source of truth; the linter surfaces drift so the
// operator can adjust the prompts.
//
// STE-flavored mode: the mechanical rules are enforced;
// the stylistic ones (active voice, plain verbs, one name
// per thing) live in the Voice Contract in SKILL.md and are
// not auto-checked. The linter is the guard rail for the
// mechanical rules.

// CONTRACTIONS is the regex the linter uses to flag
// informal contractions. The list is conservative — only
// common English contractions. The match is case-insensitive.
// The narrator's voice contract says "do not write 'it's'";
// the linter enforces it.
const CONTRACTIONS =
  /\b(don't|doesn't|didn't|isn't|aren't|wasn't|weren't|won't|wouldn't|can't|couldn't|shouldn't|mustn't|needn't|it's|that's|there's|here's|what's|who's|where's|when's|how's|I'm|you're|he's|she's|we're|they're|I've|you've|we've|they've|I'd|you'd|he'd|she'd|we'd|they'd|I'll|you'll|he'll|she'll|we'll|they'll|let's|that's|who's|what's|here's|there's)\b/gi;

// SEMICOLONS catches any semicolon. STE is strict: split
// the sentence into two. The match is intentionally
// permissive (any semicolon) — semicolons in URLs, code,
// or markdown tables are not in the review prose.
const SEMICOLONS = /;/g;

// MARKETING_ADJECTIVES is the list of words STE flags.
// The list is small and conservative. The narrator is told
// in SKILL.md to avoid them; the linter is the guard rail.
const MARKETING_ADJECTIVES = [
  "seamless",
  "seamlessly",
  "robust",
  "robustly",
  "powerful",
  "powerfully",
  "cutting-edge",
  "cutting edge",
  "effortless",
  "effortlessly",
  "world-class",
  "world class",
  "next-generation",
  "next generation",
  "revolutionary",
  "revolutionize",
  "game-changing",
  "game changing",
  "state-of-the-art",
  "state of the art",
  "leverage",
  "leveraging",
  "synergy",
  "synergies",
  "robust",
  "best-in-class",
  "best in class",
];

// MAX_SENTENCE_WORDS_INSTRUCTION is the cap for sentences
// that read like instructions. STE says 20; we round up to
// 22 to allow a small buffer (the narrator's prose is
// tighter than a procedure but not strictly STE-strict).
const MAX_SENTENCE_WORDS_INSTRUCTION = 22;

// MAX_SENTENCE_WORDS_DESCRIPTIVE is the cap for descriptive
// prose. STE says 25; same small buffer.
const MAX_SENTENCE_WORDS_DESCRIPTIVE = 27;

// lintSentenceLength splits the text into sentences (a
// best-effort regex) and reports sentences over the limit.
// The function is conservative — it does not catch every
// sentence, but it catches the egregious ones the narrator
// occasionally emits. The split is intentionally simple;
// false positives are fine (the linter warns, the narrator
// is the source of truth).
function lintSentenceLength(text) {
  const violations = [];
  // Split on `. `, `! `, `? `, or newline. This catches
  // most prose; a real sentence tokenizer would be heavier.
  const sentences = text.split(/(?<=[.!?])\s+|\n+/);
  for (const raw of sentences) {
    const trimmed = raw.trim();
    if (!trimmed) continue;
    const words = trimmed.split(/\s+/).filter(Boolean).length;
    // Heuristic: lines that start with `-` or `*` are list
    // items, not prose sentences. Skip the cap on those.
    if (/^[-*]\s/.test(trimmed)) continue;
    // Lines that are pure markdown (headings, table rows)
    // are not prose sentences. Skip the cap on those.
    if (/^#{1,6}\s/.test(trimmed)) continue;
    if (/^\|/.test(trimmed)) continue;
    if (/^```/.test(trimmed)) continue;
    if (words > MAX_SENTENCE_WORDS_INSTRUCTION) {
      violations.push({
        kind: "long-sentence",
        words,
        limit: MAX_SENTENCE_WORDS_INSTRUCTION,
        preview: trimmed.slice(0, 80),
      });
    } else if (words > MAX_SENTENCE_WORDS_DESCRIPTIVE) {
      violations.push({
        kind: "long-sentence",
        words,
        limit: MAX_SENTENCE_WORDS_DESCRIPTIVE,
        preview: trimmed.slice(0, 80),
      });
    }
  }
  return violations;
}

// lintMarketingAdjectives scans for the banned list. The
// match is case-insensitive and word-boundary. Multi-word
// entries (e.g. "cutting edge") are matched as phrases.
function lintMarketingAdjectives(text) {
  const found = [];
  for (const word of MARKETING_ADJECTIVES) {
    // Word-boundary match. For multi-word entries the
    // boundary check is on the first and last token.
    const escaped = word.replace(/\s+/g, "\\s+");
    const re = new RegExp(`\\b${escaped}\\b`, "gi");
    const matches = text.match(re);
    if (matches) {
      for (const m of matches) found.push(m);
    }
  }
  return found;
}

// lint runs the STE checks against `text` and returns a
// structured report. The runner logs the report; it does
// not modify the text.
//
// `surface` is a label the runner uses in the log line so
// the operator can tell which part of the review was
// linted (e.g. "summary" vs "inline-comment-3").
export function lint(text, surface = "text") {
  if (typeof text !== "string" || !text) {
    return { surface, violations: [] };
  }
  const violations = [];
  const contractions = text.match(CONTRACTIONS) || [];
  if (contractions.length > 0) {
    for (const c of contractions) {
      violations.push({ kind: "contraction", match: c });
    }
  }
  const semicolons = text.match(SEMICOLONS) || [];
  if (semicolons.length > 0) {
    violations.push({ kind: "semicolon", count: semicolons.length });
  }
  const marketing = lintMarketingAdjectives(text);
  for (const m of marketing) {
    violations.push({ kind: "marketing-adjective", match: m });
  }
  for (const v of lintSentenceLength(text)) {
    violations.push({ kind: v.kind, words: v.words, limit: v.limit, preview: v.preview });
  }
  return { surface, violations };
}

// lintReview is the runner-side convenience that lints
// the parsed review output. The summary + each inline
// comment body are checked; the structured-block framing
// (the `===` markers) is skipped — the runner only
// surfaces the linted prose.
export function lintReview(review, surfacePrefix = "review") {
  const reports = [];
  if (review && typeof review.summary === "string" && review.summary) {
    reports.push(lint(review.summary, `${surfacePrefix}.summary`));
  }
  if (Array.isArray(review && review.inlineComments)) {
    for (let i = 0; i < review.inlineComments.length; i++) {
      const c = review.inlineComments[i] || {};
      if (typeof c.body === "string" && c.body) {
        reports.push(
          lint(c.body, `${surfacePrefix}.inline-${i + 1}`),
        );
      }
    }
  }
  return reports;
}

// summarize returns a flat list of violations across all
// reports. Used by the run log so an operator can scan
// for STE drift in one pass.
export function summarize(reports) {
  const out = [];
  for (const r of reports || []) {
    for (const v of r.violations || []) {
      out.push({ surface: r.surface, ...v });
    }
  }
  return out;
}
