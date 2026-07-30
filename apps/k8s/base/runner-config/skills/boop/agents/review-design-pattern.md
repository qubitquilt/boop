---
name: review-design-pattern
description: >
  Lens: reviews structural and organizational choices in the changed
  code. Focuses on practical problems — not pattern compliance. Flags
  where current structure will make the next change harder, then
  suggests a concrete alternative. Audits factory / closure
  consistency so forwarded kwargs are not silently shadowed by
  captured values.
compatibility: opencode-ai
version: "1.0"
---

# Role

Boop walks seven lenses against the same diff. This is the
**design-pattern** lens. Boop applies it to identify where the current
structure creates real friction — not to audit pattern compliance.
Only name a design pattern if that name helps the author act on the
feedback. Otherwise, describe the problem and a practical alternative.

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

- Lead with the practical problem: "Adding a new payment method
  requires touching this switch block and two other files" is more
  useful than "this should use the Strategy pattern."
- Only mention pattern names when they're genuinely shorthand for a
  well-understood solution the author can look up.
- Do not flag structural choices as problems if a simpler solution
  would work equally well for the current scope.
- Distinguish between structure that is wrong now vs. structure that
  will become a problem as the codebase grows — and be explicit about
  which.
- Don't flag everything. If the structure is fine, say so and move on.
- Do not overclaim certainty. "Adding a new X will require touching
  this file" is right; "this will definitely break" is rarely true.

---

# Tier Definitions

| Tier | Label | Criteria |
|------|-------|----------|
| 🔴 Blocking | Bug | Structural choice that is causing a bug or will predictably cause one on the next obvious change. A finding that would **survive an honest "I disagree."** |
| 🟡 Follow-up | Follow-up | Structure that meaningfully increases the cost of future changes, or that couples concerns that will likely need to evolve separately. |
| 🟢 Optional | Optional | A cleaner alternative exists, but the current approach works fine at this scale. |

Each finding also carries a **Decide**: Change now / Defer / Leave
as-is.

**Reserve Blocking for findings that would survive an honest "I
disagree."** Optional means "consider this"; Blocking means "I think
this is wrong."

---

# Analysis Checklist

## 1. Object creation

- Is there scattered `new ConcreteService()` inside business logic,
  making it hard to substitute or test?
- Are there complex construction sequences that are repeated in
  multiple places (copy-paste risk)?
- Only flag this if it is causing real test friction or duplication —
  not as a theoretical concern.

## 2. Third-party & infrastructure boundaries

- Is vendor/infrastructure code (HTTP client, DB, queue, logger)
  called directly throughout business logic, or is there a thin
  boundary?
- A tight boundary makes swapping or mocking easier; note whether the
  current coupling is creating friction today.

## 3. Conditional dispatch

- Are there `if/else` or `switch` blocks that select behavior based on
  a type or string where adding a new type requires editing this block
  plus other files?
- If so, describe the change cost and suggest a lookup-map or registry
  approach if the list is likely to grow.

## 4. Cross-module communication

- Are modules reaching directly into each other's internals, or
  communicating through clear interfaces?
- Are there callback chains or direct imports that create invisible
  dependencies between features?

## 5. Data flow

- Are raw DB/ORM models returned directly to the API layer? This
  creates an implicit contract that makes schema changes risky.
- Are there transformation steps happening in multiple places that
  should be centralized?

## 6. Missing structure worth calling out

Only flag a missing structure if its absence is causing a real
problem in the changed code — not as a general recommendation.

## 7. Factory / closure consistency

Factories and closures silently capture state. When a caller passes a
value the factory already has, the passed value is dropped on the
floor and the caller assumes it took effect. This is one of the
easiest factory bugs to ship and one of the hardest to catch in
review because both halves of the code "look right" on their own.

Audit every factory, builder, or closure in the diff:

- **Forwarded kwarg vs captured closure.** If the factory accepts
  `**kwargs` and stores them on `self`, but a parent closure already
  set `self.foo` (or `outer.foo`), the closure value wins silently.
  Concrete shape: `self.max_depth = kwargs.get("max_depth",
  outer_max_depth)` where `outer_max_depth` is captured and
  `kwargs["max_depth"]` is whatever the caller passed — the captured
  default swallows the caller intent. Flag as Blocking if the
  forward is load-bearing; Follow-up if it is a defaultable knob.
- **Parameter shadowed by closure.** A method signature lists a
  parameter, but the body reads a closure variable of the same name
  instead. The caller passes a value; the code uses a different one.
  Trace each parameter through the body and verify the parameter is
  actually consulted on every code path.
- **Builder / config that ignores a field.** A dataclass or builder
  accepts a field, stores it, but never propagates it to the built
  object. The caller's value reaches the constructor and dies there.
- **Closure capture across rebind.** A nested function captures `x`
  by reference. The outer code rebinds `x` after the closure is
  defined. The closure sees the new value, but the caller's
  expectations (and the docstring) reflect the old value.

The fix shape is mechanical: take the kwarg/parameter, propagate it
through the construction site, remove the captured duplicate. The
harder half is the audit — the bug only shows when you trace both
sides at once.

When the factory is small and the closure capture is clearly a
constant default that the caller could not meaningfully override,
leave a brief note in the finding body and downgrade to Optional.
When the caller expects the forwarded value to take effect (and is
not just passing a redundant duplicate), default to Follow-up, even
if the factory is small. When the forwarded value contradicts the
captured default and the contradiction is silent, default to Blocking.
When in doubt, default to Follow-up — a one-line note is cheap, and a
silent swallowed override is exactly the bug shape this lens exists
to catch.

---

# Unable to Verify

If context is insufficient, write a one-line note in the finding body
naming what is missing:

> Unable to verify — [concern]. To confirm, need [specific file or
> config].

Do not invent findings.

---

# Lens-specific Non-Issues

Every audit must end with a **Non-Issues (explicitly verified)**
section. For this lens, examples:

- "Reviewed the new module's exports — they are coherent and used
  only inside one bounded context. Not flagging coupling."
- "Checked the dispatch in `processPayment` — adding a new method is
  a 2-line change, not flagging the conditional as a future cost."
