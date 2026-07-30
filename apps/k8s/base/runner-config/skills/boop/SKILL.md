---
name: boop
description: >
  Runs a comprehensive, multi-lens PR review using five specialized sub-agents.
  Use this skill whenever the user asks to review a PR, review a diff, review
  changed files, or conduct a code review. Triggers on phrases like "review
  this PR", "review my changes", "give me feedback on this diff", "code review",
  or "can you review". Produces a prioritized report with copy-paste-ready PR
  comments and a clear merge-readiness signal.
compatibility: Claude Code, Cowork, OpenCode
---

# PR Reviewer

A comprehensive code review skill. Reviews code like a pragmatic senior
engineer: calm, specific, non-accusatory. Assumes good intent. Focuses on
the changed code. Raises things that matter — correctness, maintainability,
coupling, readability, future change risk — and helps the author decide
whether to fix something now, defer it, or leave it alone.

---

## Install (One Command)

**Via agentskill.sh (all platforms):**
```bash
npx @agentskill.sh/cli@latest setup
```
Then in any session: `/learn @michaelruelas/boop`

**Manual install for OpenCode:**
```bash
mkdir -p ~/.config/opencode/skills && \
cp -r . ~/.config/opencode/skills/boop
```
Then restart OpenCode and invoke `@boop`.

---

## How to Use

**In Claude Code / Cowork:** Provide the PR diff (paste it, or give a file path
or GitHub URL) and say "review this PR." The skill activates automatically.

**In OpenCode:** After installing, invoke `@boop` in any session.

---

## Tone Contract

Apply this to every finding across all sub-agents:

- Observe, then explain impact. "This function handles both X and Y — if either
  needs to change independently, both paths need to move." Not: "this violates SRP."
- Prefer: "this couples two concerns", "may make future changes harder",
  "worth considering", "one thing to watch."
- Never speculate about performance without a measurement or a clear algorithmic
  reason (e.g. O(n²) in a hot loop is fair; "this might be slow" is not).
- Do not name frameworks (SOLID, GoF) unless the name genuinely helps the
  author act.
- Refactors are optional unless they fix a bug or unblock a change in flight.
- If the PR is small and clean, say so. A short positive report is a good report.

---

## Tier Definitions

Every finding carries one tier and one decide value:

| Tier          | Criteria                                                        |
|---------------|-----------------------------------------------------------------|
| 🔴 Blocking    | Correctness bug, data loss, silent failure, security issue      |
| 🟡 Follow-up   | Meaningful coupling, missing error path, will slow next reader  |
| 🟢 Optional    | Naming preference, minor cleanup, low-urgency improvement       |

**Decide:** Change now / Defer / Leave as-is

---

## Workflow

### Step 1 — Context Intake

Collect before doing anything else:

1. **PR diff / changed files** — paste, file path, or GitHub URL
2. **Language / runtime** — e.g. TypeScript + Node 20, Python 3.12
3. **PR description or ticket** — to compare intent vs. implementation
4. **Existing conventions** — linter config, style guide, README if available

If items are missing, ask once. Do not fabricate file contents or identifiers.

### Step 2 — Sub-Agent Dispatch

Load each agent file from `agents/` and run it against the same PR context.
In Claude Code / Cowork, spawn these in parallel.

| Agent file                       | Focus                                          | Output                       |
|----------------------------------|------------------------------------------------|------------------------------|
| `agents/review-code-quality.md`  | Complexity, coupling, cohesion, LOC            | `audits/code-quality.md`     |
| `agents/review-design-pattern.md`| Structural choices, practical alternatives     | `audits/design-pattern.md`   |
| `agents/review-error-handling.md`| Error paths, async safety, recovery            | `audits/error-handling.md`   |
| `agents/review-readability.md`   | Naming, clarity, signatures, magic values      | `audits/readability.md`      |
| `agents/review-solid-principles.md` | Coupling, extensibility, dependencies       | `audits/solid-principles.md` |

### Step 3 — Synthesis

1. **Deduplicate** — where multiple agents flag the same location for related
   reasons, merge into one finding citing all agent IDs.
2. **Calibrate** — before finalizing, verify every 🔴 Blocking finding is
   actually a bug or failure risk, not a style concern that drifted up.
3. **Check scope** — all findings should reference the *changed* code, not
   pre-existing issues outside the diff.

### Step 4 — Final Report

Write to `audits/boop-summary.md` using this template:

```markdown
# PR Review: [PR Title or Branch Name]
**Date:** [YYYY-MM-DD]
**Files reviewed:** [N files, +N -N]

---

## Executive Summary

[2–4 sentences. What does this PR do? Is the approach sound? What is the
one area most worth the author's attention? End with a merge-readiness
signal: ready / ready with minor changes / needs discussion on X first.]

---

## Priority Issue Table

| ID     | Tier         | File : Line      | Summary                              | Decide       |
|--------|--------------|------------------|--------------------------------------|--------------|
| CQ-001 | 🔴 Blocking   | `src/foo.ts:42`  | Off-by-one in page cursor            | Change now   |
| EH-002 | 🟡 Follow-up  | `src/bar.ts:88`  | Unhandled rejection in background job| Defer        |
| RD-003 | 🟢 Optional   | `src/baz.ts:14`  | `d` → `document` for clarity        | Leave as-is  |

---

## Categorized Findings

### Code Quality
### Structural Choices
### Error Handling
### Readability

---

## Suggested PR Comments

[3–5 comments, polite and copy-paste ready. Each leads with an observation,
explains the practical impact, and offers a concrete next step.]

### Comment 1 — [Label]
**File:** `path/to/file.ts` | **Lines:** 42–51
**Observation:** [What the code does.]
**Impact:** [What could go wrong or become harder.]
**Suggestion:**
```language
// minimal fix or sketch
```
*Decide: Change now / Defer / Leave as-is*

---

## What's Working Well

[1–3 genuine, specific observations with file and line citations.]
```

---

## Reference Files

Read the relevant `agents/` file before running each sub-agent phase.
Each file contains the full checklist and output template for that lens.

- `agents/review-code-quality.md` — complexity, coupling, cohesion, LOC
- `agents/review-design-pattern.md` — structural choices and alternatives
- `agents/review-error-handling.md` — error paths, async, recovery
- `agents/review-readability.md` — naming, clarity, signatures
- `agents/review-solid-principles.md` — coupling, extensibility, dependencies
