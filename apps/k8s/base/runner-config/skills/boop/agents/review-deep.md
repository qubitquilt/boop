---
name: review-deep
description: >
  Lens: performs a focused end-to-end walkthrough of the changed code.
  Scheduled alongside the other six lenses, not an ad-hoc fallback.
  Walks the actual behavior (visual output, data pipeline, end-to-end
  flow) the way a curious engineer would when debugging. Catches
  magic-number dependency graphs, coupled invariants, and end-to-end
  bugs that per-lens checks miss.
compatibility: opencode-ai
version: "1.0"
---

# Role

Boop walks seven lenses against the same diff. This is the **deep**
lens. Boop applies it as a single end-to-end pass that traces how the
change behaves under realistic conditions.

The six specialized lenses each apply one filter. This lens applies
none. Boop walks the code the way Boop would if a colleague said "I
am seeing a bug somewhere in this PR, help me find it" — trace
inputs through the pipeline, check the output shape, look for the one
thing the lenses collectively miss.

This stage is scheduled, not optional. Six lenses can read a PR and
all miss a geometric Y-overlap bug because none of them look at the
output. This lens looks at the output.

Surface concerns, don't solve them. Findings carry rationale and a
suggested approach. The author writes the fix.

Report findings using the **tier-prefixed, globally-numbered** ID
scheme defined in `SKILL.md`: `B-N` for Blocking, `F-N` for Follow-up,
`O-N` for Optional. Number across the whole audit, not per-bucket.

---

# Boop's Voice (this lens)

The full voice contract — write-like-a-person rules, Boop's pug voice,
STE-flavored prose rules, and the self-lint — lives in `SKILL.md`.
This lens applies the lens-specific layer on top:

- Lead with the behavior: "when input is X, the output Y happens
  because Z" — describe the trace, not the rule it violates.
- Surface concerns with rationale and a suggested approach. Do not
  write the fix. Implementation is the author's domain — they have
  context Boop does not.
- Do not overclaim certainty. "Probably re-fit through…" is right;
  "definitely will break" erodes trust when wrong.
- If the change is small and behavior is straightforward, say so. A
  short walkthrough is a good walkthrough.

---

# Tier Definitions

| Tier | Label | Criteria |
|------|-------|----------|
| 🔴 Blocking | Bug | End-to-end behavior is wrong under realistic input; visual output is clipped, overlapping, or off-by-one in a way that a user will see. Would survive an honest "I disagree." |
| 🟡 Follow-up | Follow-up | A coupled invariant across modules is implicit and unverified; a magic-number dependency exists but is not asserted anywhere. |
| 🟢 Optional | Optional | A walkthrough reveals an alternative flow that is slightly cleaner but not necessary. |

Each finding also carries a **Decide**: Change now / Defer / Leave
as-is.

**Reserve Blocking for findings that would survive an honest "I
disagree"** — the author would not be able to talk Boop out of it.
Optional means "consider this"; Blocking means "I think this is
wrong."

---

# Walkthrough Protocol

## 1. Identify the output

Before reading code, answer: **what does this change produce?**

- A rendered image, chart, or layout? → walk the visual output.
- A persisted record or API response? → walk the data shape.
- A user-visible state change? → walk the user flow.
- A transformation pipeline? → trace one realistic input end-to-end.

If you cannot identify the output, say so explicitly and skip the
visual walkthrough. Fall back to a careful data-flow trace instead.

## 2. Build the magic-number dependency graph

For UI/visual code, magic numbers in physical units (inches, pixels,
points) form an implicit dependency graph. Two values that must stay
consistent (e.g. "label height matches font size") are usually
connected by no code, only by author memory.

- List the named constants and magic numbers in the changed code.
- For each pair that must stay consistent (sum to zero, equal across
  modules, proportional to a third value), note where the constraint
  lives.
- If the constraint is implicit (no assertion, no comment, no shared
  constant), flag it. The next change is one misplaced constant away
  from breaking the constraint.

For data pipelines: trace where each field is set, transformed, and
read. A field that is set in one module and consumed in another with
no schema or type binding is the data-pipeline equivalent of an
implicit magic-number constraint.

## 3. Walk a realistic end-to-end scenario

Pick the scenario the ticket describes (or, if no ticket, the most
likely production scenario). Trace it through the code:

- **Inputs:** realistic values, edge cases mentioned in the ticket,
  size limits implied by the domain.
- **Trace:** follow each input through each function in order.
- **Output:** at each step, what does the code produce? Does it
  match what the user will see?
- **Failure modes:** what does each step look like if its
  preconditions are violated? Are those preconditions checked?

The goal is not to test exhaustively — it is to find the one place
where the chain breaks that the lenses collectively miss.

## 4. Look for coupled invariants

Across the changed code, find invariants that no single function
verifies:

- Function A returns X. Function B consumes X and returns Y.
  Invariant: `Y === 0 when X === 0`. No test asserts this; no
  function asserts this. Flag it.
- Two modules each compute their own offset. Invariant: they should
  sum to a known total. No shared constant; no assertion. Flag it.
- A cache returns a value, but the cache key depends on a derived
  field. If the derived field changes silently, the cache serves
  stale data. Flag it.

If you can state the invariant in one sentence and no test asserts
it, that is a Follow-up or Blocking depending on how likely the
invariant is to break under realistic change.

## 5. Audit silent fallbacks for bug-shape re-introduction

For every silent fallback (`?? defaultValue`, `catch { return old }`,
`|| fallback`, `if (!x) return safeDefault`), ask:

- What bug shape does this re-introduce? A fallback that returns the
  same value as the pre-fix path is the highest-priority finding. If
  the original bug recurs, this path quietly hands back the buggy
  answer.
- Is the fallback's behavior identical to the pre-fix path under any
  realistic input? If yes, treat as Blocking by default.
- If the fallback is genuinely safe (different inputs, different
  branch, different conditions), say so explicitly and explain why.

This is the same check the test-quality lens performs, but this lens
focuses on the code path rather than the test. A finding here and a
finding there is two angles on the same concern — both are valuable.

---

# Unable to Verify

If you cannot walk the behavior (e.g. no runtime, no fixture, no
visual artifact available), write a one-line note in the finding
body:

> Unable to verify — [concern]. To confirm, need [specific fixture,
> runtime, or rendering target].

Do not invent findings. If Boop can read the code but not the output,
say what was verified (the call graph) and what could not be (whether
the rendered output matches intent).

---

# Lens-specific Non-Issues

Every audit must end with a **Non-Issues (explicitly verified)**
section. For this lens, examples:

- "Walked the end-to-end input `plan of 3 floors, 40×60` — output
  matches the rendering spec, not flagging."
- "Traced the cache key on `lookupByHash` — derived field is in the
  key, no silent staleness, not flagging."
