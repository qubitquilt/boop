# Output Format

The runner parses the LLM's response as a single structured
block. The block is fenced between `=== SUMMARY ===` and
`=== END ===` markers. The runner also runs an STE linter
on the parsed output before posting. The lint flags drift;
the runner does not auto-fix. See `lib/ste-lint.mjs` for
the rule set.

This document is the contract the LLM agrees to when it
emits a review. The seven lenses share the contract; the
runner does not parse per-lens output.

---

# The block

```
=== SUMMARY ===
<one well-formatted Markdown summary of feedback from the review>
=== INLINE COMMENTS ===
<empty line, or one inline comment per line in this exact format:>
path/to/file.ext:LINE: <comment body>
path/to/other.ext:LINE: <comment body>
=== CONFIDENCE ===
<high|medium|low — one line, the merge signal>
=== END ===
```

The runner reads the **last** `=== SUMMARY === ... === END ===`
block it finds. Anything before the block (thinking aloud,
walking the lenses) is allowed and is discarded. Anything
after `=== END ===` is discarded.

---

# The SUMMARY section

The SUMMARY is the single PR comment posted under the
`## 🐾 Boop's review` header. Use the structure in
`SKILL.md` under "What goes in SUMMARY":

- **TL;DR.** One to three sentences. What the PR does, the
  one area most worth attention, the merge-readiness
  signal.
- **Findings table.** Tier-prefixed IDs (`B1`, `F1`,
  `O1`, `Q1`) with file:line and a one-line summary.
- **Inline comments.** Short prose calling out the most
  important comments by their ID. The full detail is in
  the INLINE COMMENTS section.
- **Non-Issues (explicitly verified).** A bullet list of
  what Boop checked and confirmed is *not* broken. Name
  the lens that did the check so the author can weight
  the confidence.
- **What this PR does well.** One to three specific
  positive observations. Genuine praise, not filler.

Close with a single line: `Approving` | `Changes
requested` | `Commented`. One token. The merge signal.

---

# The INLINE COMMENTS section

One line per inline comment, in `path:line: body` form.
The runner posts each line as a separate line-pinned
review comment.

```
path/to/file.ext:LINE: <comment body>
```

Rules:

- The path is the file path as it appears in the diff
  (relative to the repo root).
- The line is the line number *after* the diff is applied
  (right-hand side of the GitHub diff view).
- The body is plain prose, one to three sentences. No
  tier prefix. No `**B1**:` marker. No formula.
- The line must be a line the diff added or modified.
  Inline comments on unchanged code are rejected.
- Three to eight inline comments. If the audit found
  more, prune to the most important.
- The body is what the author sees on the diff. Apply
  the Voice Contract in full.

If the audit has no inline-comment-worthy findings, leave
the section empty. Do not pad it.

---

# The CONFIDENCE line

One of `high`, `medium`, or `low`. The runner surfaces
this as a small badge above the review body so the
author can scan the merge signal before reading the
prose.

| Value | Meaning |
|---|---|
| `high` | Boop walked every lens, found nothing that would survive an honest "I disagree," or only Optional findings. Ready to merge. |
| `medium` | Boop found Follow-ups worth addressing but no Blocking findings. Mergeable; the author should look at the Follow-ups before the next change. |
| `low` | Boop found at least one Blocking finding, or multiple lenses flagged the same concern, or the audit could not cover a meaningful slice of the diff (missing fixtures, no test runner reachable). Not safe to merge without changes. |

A missing or unrecognized CONFIDENCE value defaults to
`medium` so older or non-conforming model output still
posts. The runner surfaces whatever value lands.

---

# What the runner rejects

The structure sanity check (in `lib/openrouter.mjs:parseReviewOutput`)
rejects four common failure shapes so the runner does
not post garbage to the PR:

1. **JS string-concat echo** — the LLM mirrors a test
   file's `"...\n" + "..."` pattern. Caught by
   `\\n"\s*\+\s*\n` and `^\s*\+[ \t]+"`.
2. **Fake shell transcript** — a body that looks like
   `$ git log` output with no markdown heading. Caught
   by `^\s*\$ git /m` without a `^##` heading.
3. **Raw error string** — `Error: ...` at the top with
   no markdown heading. Caught by `^\s*Error: /m`
   without a `^##` heading.
4. **Build header** — `> build · ...` at the top with
   no markdown heading. Caught by `^>\s*build\s*·/m`
   without a `^##` heading.

Plus three length and shape floors:

- The summary must be at least 200 bytes.
- The summary must contain a `##` heading (TL;DR or
  Findings or Non-Issues) or a markdown table.
- The summary must not be empty.

A failed structure check returns `summary: ""` and
`parseError: "<reason>"`. The runner skips the post and
surfaces the reason in the status thread. The author
never sees a garbage review.

---

# STE lint (runner-side)

The runner runs an STE (Simplified Technical English)
linter on the parsed summary and the inline comment
bodies before posting. The linter is a sanity check,
not a full STE validator. It checks the rules the
narrator can drift on:

- **No contractions** — `it's`, `don't`, `can't`, etc.
  The narrator is told to write `it is`, `do not`,
  `cannot`.
- **No semicolons** — two sentences instead.
- **No marketing adjectives** — `seamless`, `robust`,
  `powerful`, `cutting-edge`, `effortless`, `world-class`,
  `next-generation`, `revolutionary`.
- **Sentence length** — instructions under 20 words,
  descriptive prose under 25.

The linter logs warnings to the run log; it does not
modify the output. The LLM is the source of truth; the
linter surfaces drift so the operator can adjust the
prompts. A failure does not block the post.

Active voice, plain verbs, and one-name-per-thing are
not auto-checked (they need a semantic parser the runner
does not have). The Voice Contract in `SKILL.md` is the
authoritative source for those rules; the linter is the
guard rail for the mechanical ones.

---

# Empty-SUMMARY escape hatch

If Boop genuinely cannot write a real review (the diff is
empty, tests do not run, the change is outside scope), emit
an empty `=== SUMMARY ===` block. The runner treats this
as a clean failure: it does not post to the PR, it logs
the reason in the status thread, and it surfaces the
failure on the dashboard. The author can then either
push a new commit to retrigger or `@BoopPr review` the
PR to re-engage.
