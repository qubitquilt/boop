# Lens File Template

Every file in `agents/review-*.md` follows this structure. The
runner inlines all lens files into one prompt, so each lens must
be self-contained. A reader (the LLM) walking the seven (now
eight) lenses in order should be able to apply each one without
re-reading the others.

---

## File structure

```yaml
---
name: <lens-name>
description: >
  Lens: <one-sentence summary of what it covers and what makes
  it distinct from the other lenses>. Use alongside the other
  lenses; this one covers <specific gap>.
version: "1.0"
---

# Role

Boop walks N lenses against the same diff. This is the
**<lens-name>** lens. Boop's standard: <one sentence defining
the quality bar — what makes a finding in this lens worth
posting>.

Surface concerns, don't solve them. Findings carry rationale
and a suggested approach. The author writes the fix.

Report findings using the **tier-prefixed, globally-numbered**
ID scheme defined in `SKILL.md`: `B-N` for Blocking, `F-N` for
Follow-up, `O-N` for Optional. Number across the whole audit,
not per-bucket.

---

# Inputs

What this lens reads that the others cannot. Default: the diff
and the surrounding changed file. If the lens needs more
(context, ticket state, dependency graph), name it here.

---

# What to flag

Numbered list of findings classes. Each class:

- One sentence naming the concrete thing the lens flags.
- One sentence describing when to flag it (threshold).
- One example or counter-example.
- The tier (`B-N`, `F-N`, `O-N`, or `Q-N` for Inquiry).

---

# What to skip

Numbered list of related things the lens does not flag. The
explicit "this is not my job" list is load-bearing — it stops
the LLM from over-firing in adjacent territory that another
lens owns.

For each, name the lens that does own it. The reader can then
read that lens's file to learn the threshold.

---

# Voice

The full voice contract — write-like-a-person rules, Boop's
pug voice, STE-flavored prose rules, and the self-lint — lives
in `SKILL.md`. This lens adds the lens-specific voice layer:
<one paragraph on what the prose for this lens looks like>.

---

# Non-Issues

A short paragraph template the lens fills in for every PR,
even when the lens has no findings. The template tells the
reader what the lens walked and what it confirmed. Example:

> Walked <N> changed files for <specific thing the lens
> checks>. Confirmed <N> instances of <thing>. No findings.

The Non-Issues section is the audit's defense against
"did you even look?" — it tells the author what was covered
so their focus can go to what was flagged.
