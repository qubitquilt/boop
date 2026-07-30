# The Boop Review Skill

The persona, voice, multi-lens workflow, and output contract for
BoopPr's review. Source of truth: the files in
`apps/k8s/base/runner-config/skills/boop/`. This page summarizes and
links.

See also: [architecture.md](./architecture.md), [product.md](./product.md),
[runner.md](./runner.md#prompt-construction-buildboopprompt),
[output format](./webhook-contract.md#output-format).

## The shape

```
boop/                                  (orchestrator)
├── SKILL.md                           voice, output spec, ID scheme
└── agents/
    ├── review-code-quality.md         lens 1
    ├── review-design-pattern.md       lens 2
    ├── review-error-handling.md       lens 3
    ├── review-readability.md          lens 4
    ├── review-solid-principles.md     lens 5
    ├── review-test-quality.md         lens 6
    └── review-deep.md                 lens 7
```

The orchestrator drives the workflow; the lens files are checklists the
orchestrator applies in one model call. They are **not** sub-agent
invocations.

## The orchestrator (`SKILL.md`)

The full source:
[`apps/k8s/base/runner-config/skills/boop/SKILL.md`](../apps/k8s/base/runner-config/skills/boop/SKILL.md).
Highlights:

### Multi-lens workflow

1. **Read the diff.** Identify every file changed and the lines added or
   modified. Note base + head SHAs; line numbers in the output refer to
   the file *after* the diff is applied (right-hand side of GitHub's
   diff view).
2. **Walk each lens** in order. For each, apply its checklist to the
   diff. Capture findings with the tier definitions and ID scheme.
3. **Synthesize.** Deduplicate across lenses. Calibrate tier. Prune to 3-8
   inline comments. Number globally in tier order.
4. **Output.** Emit the `=== SUMMARY === … === INLINE COMMENTS === …
   === END ===` block.

### Voice contract (the single most important section)

The text Boop puts on a PR is what the author sees — there is no
human-in-the-loop edit. The voice contract enforces:

- **Write like a person.** No Observation / Impact / Suggestion formula
  in real PR comments. Vary openers. Sometimes a one-liner is the right
  amount of feedback.
- **Be direct and specific.** Point to the exact line. Name the actual
  function. Explain the real-world consequence. Ask a genuine question
  when unsure.
- **Stay grounded.** Do not name frameworks (SOLID, GoF, etc.) unless
  the name helps the author act. Do not speculate about performance
  without algorithmic evidence. Do not inflate minor issues into
  architectural concerns. Treat refactors as optional unless there is a
  clear bug.
- **The pug voice.** Warmth in openings, in positives, in "I think"
  instead of "this is wrong." Not in the technical content.
- **STE-flavored prose rules.** ASD-STE100-inspired:
  - One idea per sentence, ≤20 words. Hard cap, no exceptions.
  - Active voice.
  - Plain verbs: "start" (not "begin"), "use" (not "utilize"), "make
    sure" (not "ensure"), "before" (not "prior to"), "get" (not
    "obtain"), "show" (not "demonstrate").
  - No marketing adjectives: seamless, robust, powerful, cutting-edge,
    effortless, world-class, next-generation, revolutionary — all cut.
  - No semicolons (two sentences instead).
  - No stacked auxiliaries ("it is important to note that …").
  - **No contractions in posted comments.** "do not", "is not", "it is"
    — not "don't", "isn't", "it's".
  - American spelling.
  - One name per thing in a review.
  - Em dashes: a single one for a parenthetical is fine; chains are noise.
  - Articles stay.
- **No emoji in finding bodies.** The 🐾 lives in the chrome (header,
  footer, status). Findings stay unadorned.

### Self-lint before emitting

The skill mandates a 10-point self-lint pass on every comment and the
summary before writing the SUMMARY block. Top items: long sentences
(>20 words), semicolons, contractions, passive voice, marketing
adjectives, repeated openers, "definitely will break", emoji in bodies,
"this violates X" phrasing.

### Tier definitions

| Tier | Emoji | Criteria |
|---|---|---|
| 🔴 Blocking | Bug | Correctness bug, silent failure, security issue, missing test for a real failure mode, or a silent fallback that returns the same value as the pre-fix path. **Survives an honest "I disagree."** |
| 🟡 Follow-up | Follow-up | Meaningful coupling, missing error path, mis-shaped test fixture, coupled invariant not asserted anywhere. Worth addressing soon. |
| 🟢 Optional | Optional | Naming preference, minor cleanup, low-urgency improvement. The author may legitimately ignore these. |

> **Reserve Blocking for findings that would survive an honest "I
> disagree."** Optional means "consider this"; Blocking means "I think
> this is wrong."

### ID scheme

| Prefix | Tier | Example |
|---|---|---|
| `B-N` | 🔴 Blocking | `B1`, `B2` |
| `F-N` | 🟡 Follow-up | `F1`, `F2` |
| `O-N` | 🟢 Optional | `O1`, `O2` |

Numbered **globally** across the whole audit, in tier order, regardless
of which lens produced the finding. The author can write `fix B1` in a
commit and trace it back.

### Summary structure (mandatory)

```markdown
## TL;DR

[1-3 sentences: what the PR does, the one area most worth attention,
the merge-readiness signal.]

## Findings

| ID    | Tier          | File : Line     | Summary            |
|-------|---------------|-----------------|--------------------|
| B1    | 🔴 Blocking    | `src/foo.ts:42` | Off-by-one         |
| F1    | 🟡 Follow-up   | `src/bar.ts:88` | Coupled invariant  |
| O1    | 🟢 Optional    | `src/baz.ts:14` | `d` → `document`   |

## Inline comments

[1-2 sentences highlighting the most important IDs.]

## Non-Issues (explicitly verified)

[Bulleted list of what was checked and confirmed not broken.]

## What this PR does well

[1-3 specific positives, named files and lines, no padding.]
```

Boop does not repeat the `## 🐾 Boop's review` header in the body — the
runner adds that. Boop does not repeat the footer — the runner adds
that too.

### Inline comment format

```
path/to/file.ext:LINE: <comment body>
```

One per line. The runner parses this strictly. `LINE` is a positive
integer referring to the file *after* the diff is applied.

## The seven lenses

Each lens has the same structure: Role, Voice layer, Tier Definitions,
Analysis Checklist, Unable to Verify, Lens-specific Non-Issues. Tier
criteria are the same as the orchestrator; each lens adds its own
**Decide** field (Change now / Defer / Leave as-is) on every finding.

### 1. Code quality
File: [`review-code-quality.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-code-quality.md)

Focus: complexity, coupling, cohesion, LOC. Checklists: cyclomatic
complexity (>10 only when it makes correctness hard to reason about),
cognitive complexity, function length (>50 LOC only when length is
hiding intent), file/class size (>300 / >500 LOC when size causes
unrelated concerns to live together), coupling (business logic
instantiating infrastructure directly), cohesion (`utils` / `helpers`
modules that keep growing).

### 2. Design pattern
File: [`review-design-pattern.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-design-pattern.md)

Focus: structural choices that create real friction. Checklists: object
creation (`new ConcreteService()` scattered through business logic),
third-party / infrastructure boundaries, conditional dispatch (switch
selecting by type/string), cross-module communication (modules reaching
into each other's internals), data flow (raw DB/ORM models returned to
the API layer), missing structure (only when absence is causing a real
problem).

### 3. Error handling
File: [`review-error-handling.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-error-handling.md)

Focus: error paths, async safety, failure recovery, error information
hygiene. Checklists: unhandled failure paths, error categorization
(wrong status codes — see the canonical mapping table), error info
hygiene (stack traces, internal paths, internal messages in
user-facing responses), async/concurrency (Promise.all missing catch,
fire-and-forget), recovery/resilience (retry on known-transient
external calls, SIGTERM handling), and the **silent fallback
bug-shape check** — every `?? defaultValue`, `|| fallback`, `catch {
return old }` is audited for whether it re-introduces the same bug
shape under realistic input. A fallback that returns the same value as
the pre-fix path is **Blocking by default**.

### 4. Readability
File: [`review-readability.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-readability.md)

Focus: naming, clarity, magic values, function signatures. Checklists:
intent clarity (do names communicate what they hold, not just type?),
consistency (camel/snake mix, domain terminology, abbreviations), magic
values (only flag if the purpose is not obvious from context), clarity
patterns (boolean expressions, ternaries, "what" vs "why" comments),
function signatures (>3 parameters, boolean flag parameters, return
shape obvious from name). **Most readability findings are Optional**;
a misleading name that could cause a bug is the only Blocking case.

### 5. SOLID principles
File: [`review-solid-principles.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-solid-principles.md)

Focus: coupling, extensibility, dependency structure — practical
friction, not principle compliance. **Does not name SOLID by default.**
Checklists: mixed responsibilities (one module/class/function doing
"X and Y" — only flag if the concerns are diverging or about to), hard
dispatch (switch selecting by type, requires editing this block plus
others), inheritance & substitutability (subclasses throwing where
base returns, instanceof workarounds), interface scope (unimplemented
methods, broad imports), dependency structure (`new` inside business
logic, concrete imports, DI consistency).

### 6. Test quality
File: [`review-test-quality.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-test-quality.md)

Focus: "looks-green-but-isn't" tests. Checklists:

1. **Audit test assertions against name and intent.** The canonical
   review move: "this test would still pass if the fix were reverted."
   If yes, the test does not protect the regression. Blocking.
2. **Categorize test gaps** (Missing / Mis-shaped / Weak-boundary).
   Mis-shaped is the dangerous one — a test that *looks* like
   coverage but isn't. When a ticket names a specific payload, the
   regression test must construct that artifact, not a synthetic easier
   version.
3. **Audit function composition.** Per-function tests verify each piece
   works; composition tests verify they work together. Coupled
   invariants (e.g. `f(x) + g(x) === 0` when both can return 0) that
   no per-function test would catch.
4. **Silent fallback bug-shape check** (same as lens 3; both lenses
   flag the same concern, synthesis merges into one ID).

### 7. Deep
File: [`review-deep.md`](../apps/k8s/base/runner-config/skills/boop/agents/review-deep.md)

Focus: end-to-end walkthrough, not pattern compliance. The lens
scheduled alongside the others, not a fallback. The six specialized
lenses can read a PR and all miss a geometric Y-overlap bug because
none of them look at the output. This lens looks at the output.

Walkthrough protocol: identify the output (image, persisted record, user
state, transformation pipeline). Build the magic-number dependency
graph (UI/visual: physical-unit pairs that must stay consistent but
have no shared constant; data pipeline: field set in one module and
consumed in another with no schema binding). Walk a realistic
end-to-end scenario (the one the ticket describes). Look for coupled
invariants across modules that no single function verifies. Same
silent fallback bug-shape check as lenses 3 and 6.

## Editing the skill

The skill lives in `apps/k8s/base/runner-config/skills/boop/`. Edits
require a ConfigMap regeneration, which ArgoCD applies on the next
sync. **No image rebuild required.**

To add a new lens:

1. Drop the file in
   `apps/k8s/base/runner-config/skills/boop/agents/review-<name>.md`.
2. Add a `configMapGenerator.files` entry in
   `apps/k8s/base/kustomization.yaml` using the `key=path` form so the
   directory structure is preserved on mount:
   ```yaml
   - skill-boop-agent-<name>=runner-config/skills/boop/agents/review-<name>.md
   ```
3. Add a matching `items` entry in
   `apps/receiver/internal/webhook/job-template.yaml`:
   ```yaml
   - key: skill-boop-agent-<name>
     path: skills/boop/agents/review-<name>.md
   ```
4. Add the lens to `LENS_FILES` in
   `apps/runner/src/index.mjs`:
   ```js
   "agents/review-<name>.md",
   ```
5. Add the lens to the orchestrator's table in `SKILL.md` (the
   "Multi-Lens Workflow" step 2 table).
6. Commit + push. ArgoCD syncs the ConfigMap; the next PR review uses
   the new lens.

If you only edited an existing file, steps 1-5 are skipped; the file
contents are repacked into the ConfigMap on the next sync.

The ConfigMap has a 1 MB hard cap (etcd). If you outgrow it, swap to a
git-sync init container pattern (see `apps/dev-tools/openchamber` for
the same pattern in use).

## See also

- [architecture.md](./architecture.md) — system-level flow.
- [product.md](./product.md) — the public perspective on the review.
- [runner.md](./runner.md#prompt-construction-buildboopprompt) — how the
  skill is loaded into the prompt.
- [webhook-contract.md](./webhook-contract.md#output-format) — the
  output block the runner parses.
