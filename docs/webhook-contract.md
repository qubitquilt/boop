# Webhook Contract

What the receiver accepts, how it filters, and the status thread
semantics on the PR.

See also: [receiver.md](./receiver.md), [product.md](./product.md),
[architecture.md](./architecture.md).

## Endpoint

`POST https://boop.qubitquilt.dev/webhook` (per the `pugquilt` overlay).

Headers the receiver reads:

- `X-GitHub-Event` — `pull_request` or `issue_comment` (everything else
  is `ignored`).
- `X-GitHub-Delivery` — UUID, used for log correlation. Echoed in the
  ack body.
- `X-Hub-Signature-256` — `sha256=<hex>` HMAC of the body using the
  configured `WEBHOOK_SECRET`. Verified with constant-time compare.
  Missing or invalid → 401.

Body is read up to 1 MiB (`1<<20`); over that → 400.

## Accepted events

| Event | Action filter | Effect |
|---|---|---|
| `pull_request` | `opened` | Submit a Job, post 🐾 |
| `pull_request` | any other (`reopened`, `synchronize`, `ready_for_review`, `closed`, `edited`, `assigned`, `labeled`, `unlabeled`, `review_requested`, `review_request_removed`, …) | Ack `ignored`, no Job |
| `issue_comment` | `created` on a PR (`issue.pull_request` set) + comment body matches the request grammar | Submit a Job, react 👀 (QUB-114 reaction mode); no status comment |
| `issue_comment` | `created` on a plain issue | Ack `ignored` |
| `issue_comment` | `created` from a sender matching `BOT_LOGIN` (when set) | Ack `ignored` (self-mention) |
| `issue_comment` | `created` on a PR but body does not match the request grammar | Ack `ignored` |
| `issue_comment` | any action other than `created` | Ack `ignored` |
| anything else | — | Ack `ignored` (logged at debug) |

## Request grammar

`requestsReview(body)` is true when the body contains `@BoopPr` (whole
token, case-insensitive) followed by an explicit review request. The
regex:

```regex
(?i)@BoopPr\b,?\s+(?:please\s+|to\s+|can\s+(?:you\s+)?|could\s+(?:you\s+)?|will\s+(?:you\s+)?|may\s+(?:you\s+)?)?(?:re-)?review\b
```

Matched (`true`):

```
@BoopPr review
@BoopPr, review
@BoopPr review please
@BoopPr please review
@BoopPr to review
@BoopPr can review
@BoopPr can you review
@BoopPr could you review
@BoopPr will you review
@BoopPr may you review
@BoopPr re-review
@BoopPr, re-review this
@BoopPr, can you re-review this
hey @BoopPr please review
Multi line\n@BoopPr review\nthanks
@booppr REVIEW
```

Not matched (`false`):

```
@BoopPr hi
@BoopPr are you awake?
@BoopPr look at this code review carefully   (reference, not request)
@BoopPr the prior review was great           (reference)
@BoopPr I left a review on PR #1 yesterday   (reference)
@BoopPr this needs another look first        (no "review" verb)
@BoopPr-bot review                           (not a whole-token match)
@BoopPrbot review                            (adjacent alnum)
@BoopPr2 review                              (adjacent digit)
@booppr_bot review                           (adjacent underscore)
nothing here
""
```

The whole-token boundary (`@BoopPr\b`) plus the explicit verb means bare
mentions and references to prior reviews are ignored.

## Job naming and dedup

The Job name encodes the head SHA:

```
boop-<owner>-<repo>-<number>-<sha7>
```

Sanitized to `[a-z0-9-]`; owner / repo lowercased. Examples:

- `boop-qubitquilt-homelab-infra-42-a1b2c3d`
- `boop-michaelruelas-homelab-infra-42-abc1234`

The K8s API is the source of truth for "is there already a run for this
head":

| Existing Job | Action |
|---|---|
| missing | Submit a new Job. |
| active | Ack `duplicate`. No new Job, no new status comment, no reaction. |
| succeeded | Ack `duplicate`. For `issue_comment` triggers, post a short "Already sniffed `<sha>`" reply. For `pull_request` triggers, no PR reply (the run already produced one). |
| failed | Delete the failed Job (background propagation), submit a new one. |

This runs **before** any external side effect (status comment,
reaction, PR reply) so a duplicate delivery cannot leave stranded
🐾 comments, orphan status threads, or spurious 👀 reactions on
the trigger comment.

## Status thread

The receiver pre-creates the status comment with a header that encodes
who triggered and which review this is:

> 🐾 **Boop's on the case!** (review)
>
> Last commit: `a1b2c3d`. Sniffing now — updates will appear here.
>
> <!-- boop-timeline -->

For `issue_comment` triggers, the "Triggered by @<user>" line is added
between the label and the commit line. For re-reviews (review #2 and
beyond), the label becomes "re-review #N".

The runner PATCHes the comment at each stage. The PATCH is **append-only**
on the runner's side: a `<!-- boop-timeline -->` separator splits the
receiver-supplied header from the runner's timeline. The runner never
modifies the header (label, trigger attribution, commit).

Stages and emojis (must match across receiver and runner):

| Stage | Emoji | Short label | Source of body |
|---|---|---|---|
| (header) | 🐾 | `🐾 Boop's on the case!` | receiver `renderStatusBody(StatusInitial, …)` — the initial header the runner appends the timeline below |
| `auth` | 🤝 | `🤝 paw-shaken in` | receiver `renderStatusBody(StatusAuth, …)` (PATCH replaces header status line) |
| `clone` | 🥎 | `🥎 fetched` | receiver `renderStatusBody(StatusClone, …)` |
| `review` | 👃 | `👃 sniffing` | receiver `renderStatusBody(StatusReview, …)` |
| `done` | 🦴 | `🦴 bone delivered` | runner `STATUS.done` (PATCH after summary posted) |
| `failed` | ❌ | `❌ lost the bone` | runner `STATUS.failed` (with a `<details>` block carrying the error tail) |

The receiver's `StatusInitial` is the body the comment is created with
(no timeline yet). The runner's first PATCH (`auth`) appends
`- 🤝 paw-shaken in` after the separator. The final PATCH (`done` or
`failed`) appends the closing stage.

Body trim: the runner keeps the combined body under 60 KB. If the
cumulative timeline would push it over, the runner slices at the
separator and trims the oldest entries, keeping the most recent 58 KB.

### Reaction mode (QUB-114)

`issue_comment` triggers skip the entire status-comment path. The
receiver does not call `renderStatusBody` or post a 🐾 comment;
instead it `AddCommentReaction(👀)` on the trigger comment and sets
`BOOP_NO_STATUS_COMMENT=1` on the Job. The runner's `postStatus`
wrapper is a no-op the whole way through, and `postFinalReaction`
adds a single terminal reaction on the trigger comment when the
review finishes:

| Run result | Final reaction | Resulting transition |
|---|---|---|
| succeeded | 🎉 | 👀 → 🎉 |
| failed | 👎 | 👀 → 👎 |

The author's view is one reaction change, one notification. No
PATCH loop on a status comment that never existed. Pull-request
triggers (`pull_request` `opened`) keep the status-thread path
above unchanged.

## Review number

`computeReviewNumber` counts existing Boop summary comments on the PR
(via `CountPriorReviews`) and adds 1. The result is:

- `1` → label is `review` (no `re-review`).
- `>1` → label is `re-review #N`, where N is the new total.
- On GitHub API error → falls back to 1 so a transient hiccup never
  blocks a review.

The number is passed to the runner as `BOOP_REVIEW_NUMBER` and used for
the summary comment's H2 header:

| n | Header |
|---|---|
| 0, 1, undefined, null | `## 🐾 Boop's review` |
| 2 | `## 🐾 Boop's re-review #2` |
| 3 | `## 🐾 Boop's re-review #3` |
| 10 | `## 🐾 Boop's re-review #10` |

Format is identical on both sides
(`apps/receiver/internal/github/client.go` `ReviewSummaryHeader` and
`apps/runner/src/review-header.ts` `reviewHeader`). Tests pin both.

## Summary comment

Posted by the runner. The body is the SKILL.md `SUMMARY` content. The
runner wraps it with:

```
## 🐾 Boop's review      (or "## 🐾 Boop's re-review #N")

<body the model produced>

<sub>Posted by [BoopPr](https://github.com/qubitquilt/boop) · PR `<sha7>` · [review #N · ]good boy powered</sub>
```

The body is trimmed to 65 KB (GitHub's hard cap on issue comments) by
appending `…(truncated)` if needed.

## Inline comments

Posted by the runner. Each line of the model's `INLINE COMMENTS` block
becomes a `POST /repos/{owner}/{repo}/pulls/{n}/comments` with:

- `commit_id`: the head SHA
- `path`: file path
- `line`: 1-based line number (right-hand side of the diff)
- `side`: `"RIGHT"`
- `body`: the comment body

Each inline is independent (a "pending review" with one comment), so a
single failure does not block the rest. Failures are logged per inline.

The skill instructs the model to:

- Use line numbers that refer to the file *after* the diff is applied.
- Only comment on lines that were added or modified in the PR.
- Output 3-8 inline comments; prune beyond that.

The runner does not validate that `path` and `line` are within the diff
(Octokit returns 422 if not). It catches and logs per-inline failures.

## Output format (what the model must emit)

The runner parses the **last** block the model emits. Format is exact:

```
=== SUMMARY ===
<markdown body — the structure defined in skills.md>
=== INLINE COMMENTS ===
<empty line, or one inline comment per line, in path:line: body form>
=== END ===
```

Regex (case-insensitive, tolerant of whitespace):

```regex
===\s*SUMMARY\s*===\s*([\s\S]*?)\s*===[\s\S]*?INLINE COMMENTS\s*===\s*([\s\S]*?)\s*===\s*END\s*===
```

Rules:

- Anything before `=== SUMMARY ===` (the TUI prompt, bash transcripts,
  lens walks, etc.) is dropped by the runner. The model can think out
  loud before the block.
- Anything after `=== END ===` is dropped. Do not write prose after.
- The `INLINE COMMENTS` section is parsed line-by-line. Lines that
  don't match `^(\S+?):(\d+):\s+(.*)$` are skipped. Empty lines are
  ignored.
- If the structured block is missing, the runner falls back to the
  whole stdout as the summary (so a malformed output still produces
  *something*).
- ANSI escape sequences are stripped before parsing.

## Self-mention handling

If `BOT_LOGIN` is set in the receiver's env, `issue_comment` events
from a sender matching that login are dropped at the receiver. This
prevents Boop reacting to its own 🐾 status comment when a human (or
the runner, via Octokit) posts back to the PR.

Default `BOT_LOGIN` in `apps/k8s/base/deployment.yaml` is `BoopPr[bot]`,
the GitHub App's bot identity.

## Re-delivery from GitHub

GitHub retries webhooks on 5xx and on connection errors. The receiver
always returns 202 (with `status: ignored` for events it doesn't care
about), so a re-delivery is a no-op except in the dedup path, where the
second arrival sees the existing Job and acks `duplicate`.

## See also

- [receiver.md](./receiver.md) — implementation.
- [skills.md](./skills.md) — the boop skill and the SUMMARY structure.
- [product.md](./product.md) — what the author sees.
