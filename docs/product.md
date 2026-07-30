# Product Surface

> The end-user perspective. What a PR author sees when BoopPr reviews their PR.

This is the contract BoopPr offers to humans opening pull requests in the
`qubitquilt` org. Everything inside `boop` is implementation; this page is
the surface.

## Triggers

BoopPr reviews in two ways:

1. **Automatic on PR lifecycle events.** `opened`, `reopened`, `synchronize`
   (new push), `ready_for_review`. Boop sees the PR, posts a status comment,
   and starts a review.
2. **On demand.** Comment on any PR (the issue, a review, the conversation
   tab) with `@BoopPr review` (and a small set of natural phrasings). Boop
   re-runs against the current head SHA. See the [request grammar](./webhook-contract.md#request-grammar)
   for the full set.

   Bare mentions (`@BoopPr hi`, `@BoopPr look at this code review`) do
   **not** trigger a run.

## What the author sees on the PR

A typical review produces three things on the PR's conversation tab, in order:

### 1. Status comment (the "reviewing…" thread)

Posted up front by the receiver. PATCHed by the runner at each stage. Looks
like:

> 👀 **boop is reviewing this PR...** (review)
>
> Last commit: `a1b2c3d`. Updates will appear here.
>
> <!-- boop-timeline -->
> - 🔐 authenticated
> - 📥 fetched
> - 🧠 reviewing
> - ✅ review in

A re-review (second-or-later run on the same PR) shows up as:

> 👀 **boop is reviewing this PR...** (re-review #2)
>
> Triggered by @alice
>
> Last commit: `d4e5f6a`. Updates will appear here.

The header carries the review label so users can tell at a glance which
run produced which comments. Each re-review gets its own status thread.

If the review fails, the timeline ends with a "details" block containing
the runner's error tail.

### 2. Summary comment (the headline)

One comment, H2 header `## 🐾 Boop's review` (or `## 🐾 Boop's re-review #N`
for runs after the first), with a fixed structure:

```markdown
## TL;DR

[1-3 sentences: what the PR does, the one area most worth attention, the
merge-readiness signal: ready / ready with minor changes / needs discussion
before merging.]

## Findings

| ID    | Tier          | File : Line     | Summary                |
|-------|---------------|-----------------|------------------------|
| B1    | 🔴 Blocking    | `src/foo.ts:42` | Off-by-one in cursor   |
| F1    | 🟡 Follow-up   | `src/bar.ts:88` | Coupled invariant…     |
| O1    | 🟢 Optional    | `src/baz.ts:14` | `d` → `document`       |

## Inline comments

[1-2 sentences highlighting the most important inline comment IDs.]

## Non-Issues (explicitly verified)

[Bullet list: what Boop checked and confirmed is not broken.]

## What this PR does well

[1-3 specific positives. Named files, named lines, no padding.]
```

A footer (added by the runner, not by the skill) reads:

> <sub>Posted by [BoopPr](https://github.com/michaelruelas/homelab-infra) ·
> PR `a1b2c3d` · review #2 · good boy powered</sub>

### 3. Inline comments (line-specific)

0-8 review comments, each pinned to a line on the diff's right-hand side
(after the change is applied). Format: plain prose, 1-3 sentences, no tier
prefix, no formula, no emoji. They render as native GitHub review comments
on the file diff.

Boop only comments on lines that were **added or modified** in the PR. No
comments on unchanged code.

## Tier system (what Blocking / Follow-up / Optional mean)

| Tier | Emoji | What it signals | Author action |
|------|-------|-----------------|---------------|
| Blocking | 🔴 | Correctness bug, silent failure, security issue, missing test for a real failure mode, silent fallback that re-introduces a bug shape. | Address before merge. |
| Follow-up | 🟡 | Missing error path, coupled invariant unverified, mis-shaped test fixture, suspicious fallback. | Worth addressing soon; fine to merge with a follow-up ticket. |
| Optional | 🟢 | Naming preference, minor cleanup, low-urgency improvement. | Consider; the author may legitimately ignore these. |

Every finding gets a stable, globally-numbered ID (`B1`, `F1`, `O1`…). The
author can write `fix B1` in a commit message and trace it back to the review.

## Voice contract (what Boop will never say)

The text Boop puts on a PR is what the author sees — there is no human
edit step. The voice contract enforces:

- **No slop.** No "seamless", "robust", "powerful", "cutting-edge",
  "effortless", "world-class", "next-generation", "revolutionary". No
  marketing adjectives.
- **Plain verbs.** "start" (not "begin"), "use" (not "utilize"), "make
  sure" (not "ensure"), "before" (not "prior to"), "get" (not "obtain").
- **No semicolons, no em-dash chains.**
- **No contractions in posted comments.** "do not", not "don't".
- **American spelling.**
- **No emoji in finding bodies.** The 🐾 lives in the chrome — headers,
  status comments, footer. Findings stay unadorned so they read as serious.
- **One idea per sentence.** ≤25 words.
- **No "Observation / Impact / Suggestion" formula.** Real prose, varied
  openers, sometimes one sentence.
- **No "definitely will break."** "Probably…" is right; "definitely…"
  is rarely true.

Full list with rationale:
[`apps/k8s/base/runner-config/skills/boop/SKILL.md`](../apps/k8s/base/runner-config/skills/boop/SKILL.md).

## What Boop will not do

- **Write the fix.** Findings carry rationale and a suggested approach. The
  author writes the patch — they have context Boop does not.
- **Comment on every line.** A nitpick on every line is noise. 3-8 of the
  most important findings, max.
- **Catch optional cleanups as follow-ups.** Optional is its own bucket;
  the author may ignore it.
- **Overclaim certainty.** "Probably re-introduces the same bug shape" is
  right; "definitely will break" erodes trust when wrong.
- **Approve or block the PR.** Boop posts a review; humans decide merge.

## Re-review semantics

A re-review (push a new commit, or comment `@BoopPr review` on the same PR)
is a fresh run. Findings get a new review number (`review #2`,
`re-review #3`). Each re-review:

- Counts as a new `## 🐾 Boop's re-review #N` summary.
- Posts its own status comment with a "re-review #N" label.
- Does not edit or delete prior reviews.
- Is deduped at the head-SHA level — pushing the same SHA twice
  (`@BoopPr review` on a SHA that already has a `succeeded` Job) gets a
  short "Already sniffed `a1b2c3d`" reply instead of a new run.

## How to ask for a re-review

Anything that matches the [request grammar](./webhook-contract.md#request-grammar)
works. The canonical form is:

> @BoopPr review

Also accepted (case-insensitive): `@BoopPr please review`, `@BoopPr, can
you review this`, `@BoopPr re-review`, `@BoopPr to review`.

## Limits

- **Comment body ≤ 65,000 chars** (GitHub hard limit on issue comments).
  The runner trims the summary if it goes over.
- **Inline comment count ≤ 8.** The skill prunes to the most important
  findings beyond that.
- **Active deadline: 30 min** per Job. A hung model call is hard-killed at
  25 min; post-review work has 5 min of headroom.
- **Backoff: 1 retry** per Job (the K8s `backoffLimit`). On persistent
  failure, a fresh Job replaces the failed one automatically.
- **Job TTL after finish: 1 hour.** The Job (and its pod) is GC'd an hour
  after completion so the namespace does not fill up.

## What the author should *not* expect

- Boop does not look at the deployment target. It reviews the code on the
  PR head; production behavior is the author's responsibility.
- Boop does not run tests or build the code. It reads the diff. A test
  suite that has not been run is out of scope.
- Boop does not see CI results. The summary's "merge readiness" signal
  reflects code review only.
- Boop does not see private comments, draft PRs, or PRs the App has not
  been installed on. The App is installed on the `qubitquilt` org.

## If something goes wrong

- **No 👀 comment appeared.** Boop is not installed, the webhook is
  misconfigured, or the receiver is down. Check the receiver logs.
- **👀 but no review.** The Job is queued, running, or failed. Read the
  Job's pod logs (`kubectl logs -n dev-tools -l app=boop,pr-number=N`).
- **Review is wrong / overclaims / rude.** File an issue. The voice
  contract and tier definitions in
  [`apps/k8s/base/runner-config/skills/boop/SKILL.md`](../apps/k8s/base/runner-config/skills/boop/SKILL.md)
  are the source of truth; we tune the model to match.
- **Boop reviewed the wrong SHA / stale code.** Re-push or comment
  `@BoopPr review` on the latest head.
