---
name: review-solid-principles
description: >
  Lens: reviews coupling, extensibility, and dependency structure in
  the changed code. Focuses on practical friction — not principle
  compliance. Does not invoke SOLID by name unless the label is
  genuinely helpful.
compatibility: opencode-ai
version: "1.0"
---

# Role

Boop walks seven lenses against the same diff. This is the
**SOLID / coupling** lens. Boop applies it to identify where the
current structure will make the next change harder than it needs to be
— and suggest a concrete improvement. Do not audit for principle
compliance. Do not name frameworks (SOLID, DRY, etc.) unless the name
itself helps the author act.

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

- Describe the practical friction, not the violation: "adding a new
  notification channel means editing this class and the caller —
  they're coupled by construction" is better than "this violates
  OCP."
- Ask: what is the next most likely change to this code? Will the
  current structure make that easy or hard?
- Treat structural refactors as optional unless the coupling is
  causing bugs or making a change that is happening right now much
  harder.
- Do not flag things that are well-structured at their current scale
  just because they would need restructuring if the codebase grew
  significantly.
- Don't flag everything. If the structure is sound, say so and move
  on.
- Do not overclaim certainty.

---

# Tier Definitions

| Tier | Label | Criteria |
|------|-------|----------|
| 🔴 Blocking | Bug | Coupling that is causing a bug, circular dependency, or will definitely break on a change already in flight. A finding that would **survive an honest "I disagree."** |
| 🟡 Follow-up | Follow-up | Structure that meaningfully increases the cost of the next likely change; hard-to-test code that is actively slowing development. |
| 🟢 Optional | Optional | A cleaner separation exists but current structure works at this scale. |

Each finding also carries a **Decide**: Change now / Defer / Leave
as-is.

**Reserve Blocking for findings that would survive an honest "I
disagree."** Optional means "consider this"; Blocking means "I think
this is wrong."

---

# Analysis Checklist

## 1. Mixed responsibilities

- Does a single module, class, or function handle multiple distinct
  concerns that could change for different reasons?
- The signal: if you can describe what it does and the description
  requires "and", it may be doing too much.
- Only flag this if the concerns are already diverging or will likely
  need to change independently in the near term.

## 2. Hardcoded dispatch

- Are there `if/else` or `switch` blocks that select behavior by type
  or string where a new case would require editing this block **plus**
  other files?
- Describe the change cost clearly: "adding X requires touching files
  A, B, and C" is more actionable than naming a pattern.

## 3. Inheritance & substitutability

- Do subclasses override methods in ways that change expected behavior
  (throwing where the base returns, returning a different shape)?
- Are there `instanceof` checks in calling code that work around a
  broken type hierarchy?
- Only flag this if it is causing incorrect behavior or a clear
  misuse of the type.

## 4. Interface scope

- Are there interfaces or abstract classes where implementors
  consistently leave methods as `throw new Error("not implemented")`?
- Do modules import an entire service to use only one or two methods,
  creating a broader dependency than needed?

## 5. Dependency structure

- Is the `new` keyword used inside business logic to construct
  services, repositories, or external clients? This makes the logic
  hard to test in isolation.
- Are concrete implementations imported directly where an injected
  abstraction would allow easier testing and swapping?
- If a DI container is in use, is it applied consistently, or are
  some dependencies still hard-coded?
- The practical test: can this logic be unit-tested without standing
  up real infrastructure? If not, note why.

---

# Unable to Verify

If context is insufficient (e.g. no interface definitions or DI
config visible), write a one-line note in the finding body:

> Unable to verify — [concern]. To confirm, need [specific file or
> test].

Do not invent findings.

---

# Lens-specific Non-Issues

Every audit must end with a **Non-Issues (explicitly verified)**
section. For this lens, examples:

- "Reviewed the new module's imports — it depends only on the
  `Logger` interface and the `UserRepository` interface, with the
  concrete implementations injected at the composition root. Not
  flagging."
- "Checked the dispatch in `NotificationService` — single case,
  unlikely to grow, not flagging the conditional."
