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

const CONTRACTIONS =
  /\b(don't|doesn't|didn't|isn't|aren't|wasn't|weren't|won't|wouldn't|can't|couldn't|shouldn't|mustn't|needn't|it's|that's|there's|here's|what's|who's|where's|when's|how's|I'm|you're|he's|she's|we're|they're|I've|you've|we've|they've|I'd|you'd|he'd|she'd|we'd|they'd|I'll|you'll|he'll|she'll|we'll|they'll|let's|that's|who's|what's|here's|there's)\b/gi;

const SEMICOLONS = /;/g;

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
] as const;

const MAX_SENTENCE_WORDS_INSTRUCTION = 22;
const MAX_SENTENCE_WORDS_DESCRIPTIVE = 27;

type Violation =
  | { kind: "contraction"; match: string }
  | { kind: "semicolon"; count: number }
  | { kind: "marketing-adjective"; match: string }
  | { kind: "long-sentence"; words: number; limit: number; preview: string };

export type LintReport = {
  surface: string;
  violations: Violation[];
};

function lintSentenceLength(text: string): Violation[] {
  const violations: Violation[] = [];
  const sentences = text.split(/(?<=[.!?])\s+|\n+/);
  for (const raw of sentences) {
    const trimmed = raw.trim();
    if (!trimmed) continue;
    const words = trimmed.split(/\s+/).filter(Boolean).length;
    if (/^[-*]\s/.test(trimmed)) continue;
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

function lintMarketingAdjectives(text: string): string[] {
  const found: string[] = [];
  for (const word of MARKETING_ADJECTIVES) {
    const escaped = word.replace(/\s+/g, "\\s+");
    const re = new RegExp(`\\b${escaped}\\b`, "gi");
    const matches = text.match(re);
    if (matches) {
      for (const m of matches) found.push(m);
    }
  }
  return found;
}

export function lint(text: string | null | undefined, surface: string = "text"): LintReport {
  if (typeof text !== "string" || !text) {
    return { surface, violations: [] };
  }
  const violations: Violation[] = [];
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
    violations.push(v);
  }
  return { surface, violations };
}

export function lintReview(review: import("../types.ts").Review | null | undefined, surfacePrefix: string = "review"): LintReport[] {
  const reports: LintReport[] = [];
  if (review && typeof review.summary === "string" && review.summary) {
    reports.push(lint(review.summary, `${surfacePrefix}.summary`));
  }
  if (review && Array.isArray(review.inlineComments)) {
    for (let i = 0; i < review.inlineComments.length; i++) {
      const c = review.inlineComments[i];
      if (!c) continue;
      if (typeof c.body === "string" && c.body) {
        reports.push(lint(c.body, `${surfacePrefix}.inline-${i + 1}`));
      }
    }
  }
  return reports;
}

export function summarize(reports: LintReport[] | null | undefined): Array<{ surface: string } & Violation> {
  const out: Array<{ surface: string } & Violation> = [];
  for (const r of reports || []) {
    for (const v of r.violations || []) {
      out.push({ surface: r.surface, ...v });
    }
  }
  return out;
}
