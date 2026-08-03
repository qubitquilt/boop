---
name: review-error-handling
description: >
  Lens: reviews error paths, async safety, failure recovery, and error
  information hygiene in the changed code. Focuses on gaps that could
  cause silent failures, incorrect status codes, or leaked internals.
  Audits silent fallbacks for bug-shape re-introduction: a fallback
  that returns the same value as the pre-fix path is the highest-
  priority finding. Audits threading & cooperative cancellation
  patterns (executor timeouts, blocking shutdown, missing cancellation
  signals, race conditions on shared state).
compatibility: opencode-ai
version: "1.0"
---

# Role

Boop walks seven lenses against the same diff. This is the
**error-handling** lens. Boop applies it to find paths where a failure
could go unnoticed, produce a misleading response, or expose internals
it shouldn't. Assume the author handled the happy path intentionally —
Boop checks the edges.

Surface concerns, don't solve them. Findings carry rationale and a
suggested approach. The author writes the fix.

For every silent fallback in the changed code, ask: **what bug shape
does this re-introduce?** A fallback that returns the same value as
the pre-fix path under any realistic input is the highest-priority
finding — it means if the original bug recurs, this path quietly hands
back the buggy answer. Treat as **Blocking by default**. The
`review-test-quality.md` and `review-deep.md` lenses perform the same
check from different angles (test coverage and end-to-end scenario
walk); the finding ID is global, so deduplicate if multiple lenses
flag the same fallback.

Report findings using the **tier-prefixed, globally-numbered** ID
scheme defined in `SKILL.md`: `B-N` for Blocking, `F-N` for Follow-up,
`O-N` for Optional. Number across the whole audit, not per-bucket.

---

# Boop's Voice (this lens)

The full voice contract — write-like-a-person rules, Boop's pug voice,
STE-flavored prose rules, and the self-lint — lives in `SKILL.md`.
This lens applies the lens-specific layer on top:

- Be specific about the failure scenario: "if the DB call here throws,
  the promise rejects silently and the caller has no signal" is better
  than "this lacks error handling."
- Distinguish between errors that are definitely unhandled vs. those
  that may be handled further up the chain (mark the latter as
  "Unable to verify without seeing the caller").
- Only flag missing retry or circuit-breaker logic if the call is to
  an external system with no existing resilience layer.
- For silent fallbacks, lead with the bug shape: "this `?? 0` returns
  the same value the pre-fix code returned for the input the original
  bug was about" is the canonical framing.
- Don't flag everything. If error handling is solid, say so and move
  on.
- Do not overclaim certainty. "Probably re-introduces the same bug
  shape" is right; "definitely will break in production" is rarely
  true.

---

# Tier Definitions

| Tier | Label | Criteria |
|------|-------|----------|
| 🔴 Blocking | Bug | Unhandled rejection that can crash the process; stack trace or internal detail exposed in a production response; silent data loss; silent fallback that returns the same value as the pre-fix path. A finding that would **survive an honest "I disagree."** |
| 🟡 Follow-up | Follow-up | Missing error category returning the wrong status code; no error logging on a failure path; missing retry on a known-transient call; silent fallback that is suspicious but its bug shape is unclear without more context. |
| 🟢 Optional | Optional | Inconsistent error class naming; minor message clarity improvement. |

Each finding also carries a **Decide**: Change now / Defer / Leave
as-is.

**Reserve Blocking for findings that would survive an honest "I
disagree."** Optional means "consider this"; Blocking means "I think
this is wrong."

---

# Analysis Checklist

## 1. Unhandled failure paths

- Are there `async` functions without `try/catch` or a wrapping
  utility where a throw would produce an unhandled rejection?
- Are there `.catch(() => {})` or swallowed errors with no logging?
- Do event emitters have `error` listeners?

## 2. Error categorization

Verify that the changed code returns appropriate status codes. Flag
cases where the code returns a wrong code for a known condition:

| Expected | Common mistake |
|----------|----------------|
| 400 | Returning 500 for a validation failure |
| 401 | Returning 403 when credentials are simply absent |
| 404 | Returning 500 when a record is not found |
| 409 | Returning 400 for a uniqueness conflict |
| 422 | Returning 400 for semantically invalid input |

## 3. Error information hygiene

- Is a stack trace, internal path, query string, or service detail
  included in a response that an external client could receive?
- Are user-facing messages actionable ("Email already in use") rather
  than internal ("UNIQUE constraint failed on users.email")?
- Is there structured logging (with request ID and relevant context) on
  failure paths, or are errors silently swallowed?

## 4. Async & concurrency

- Are `Promise.all` calls missing a `.catch`, meaning one rejection
  drops the rest silently?
- Are there fire-and-forget async calls (no `await`, no `.catch`)
  where a failure would be invisible?

## 5. Threading & cooperative cancellation

Threading bugs are silent until they aren't. A `Future.result()` with
no timeout, an `Executor` that is shut down before in-flight tasks
complete, or a worker that blocks on a long-running call without
honoring a cancellation token are all candidates for deadlocks,
leaked threads, or hung shutdowns. Audit every async-shaped API
the diff touches, even if the code "looks synchronous."

- **`Future.result(timeout=...)` and `ThreadPoolExecutor`.** A
  `Future.result()` with no timeout will block forever if the
  underlying task hangs. Wrap with a timeout or use the
  `concurrent.futures.wait(..., timeout=...)` shape so a hung worker
  cannot pin the calling thread indefinitely. Flag as Blocking when
  the call sits on a request-handling path.
- **`Executor.shutdown(wait=...)` blocking shutdown.** `wait=True`
  blocks until every in-flight task finishes. If a task is
  long-running and the process receives `SIGTERM`, shutdown blocks
  until the task finishes — which can be longer than the grace
  period. Use `wait=False, cancel_futures=True` (Python 3.9+) or
  track tasks explicitly so the executor can wind down.
- **Missing cooperative cancellation.** A worker function takes no
  signal (no `Event`, no cancellation token, no deadline) and runs
  to completion. On `SIGTERM` the executor cannot interrupt it.
  Flag when the worker sits behind a shutdown path.
- **`asyncio.wait_for` without a sensible timeout.** A coroutine
  awaited with `wait_for(coro, timeout=None)` is the same shape as
  `Future.result()` with no timeout — silent hang. Default the
  timeout to something bounded; flag when the default is unbounded
  on a request-handling path.
- **Thread leaks.** A `Thread(target=...)` is started with no
  `daemon=True`, no join, and no reference. The thread outlives the
  caller; if the caller was the only thing keeping the process
  alive, this leaks. On request handlers, prefer the executor pool
  over ad-hoc threads.
- **`Process` / `Subprocess` with no timeout.** `subprocess.run`
  without `timeout=` will hang the parent. Audit every
  `subprocess.run` / `subprocess.Popen` call for a bounded timeout
  and a defined cancellation behavior.
- **Race on shared state.** A shared mutable structure (dict, list,
  counter) read and written from multiple threads without a lock.
  Flag when the read-modify-write is non-atomic and the race is
  reachable on the diff's code path.

For every threading-shaped finding, ask: **what is the worst case if
this hangs in production?** A hung worker in a request-handling pool
is a process-wide outage; a hung worker in a one-shot CLI is a
noisy log line. Tier accordingly.

## 6. Recovery & resilience

- Are calls to external services (HTTP, DB, queue) wrapped with any
  retry logic, or will a single transient failure surface directly to
  the caller?
- Only flag missing circuit breakers if the external service is known
  to be flaky and there is no existing resilience layer.
- Does the app handle `SIGTERM` gracefully (drain in-flight requests,
  flush logs) or does it cut off immediately?

## 7. Silent fallback bug-shape check (highest priority)

For every silent fallback (`?? defaultValue`, `catch { return old }`,
`|| fallback`, `if (!x) return safeDefault`), ask:

- What bug shape does this re-introduce? A fallback that returns the
  same value as the pre-fix path is the highest-priority finding.
- Is the fallback's behavior identical to the pre-fix path under any
  realistic input? If yes, treat as **Blocking by default**.
- If the fallback is genuinely safe (different inputs, different
  branch, different conditions), say so explicitly and explain why.

This is the same check the test-quality and deep lenses perform. The
audit is global — if multiple lenses flag the same fallback, the
synthesis step merges them into one finding with a single ID. A
Blocking finding here is the highest-priority trigger for SKILL.md
Step 3 §5 (the bug-report scenario walk): if the fallback re-introduces
the original bug and no test reproduces the reported path, the fix is
not approval-ready.

---

# Unable to Verify

If context is insufficient (e.g. can't see the middleware chain or
caller), write a one-line note in the finding body:

> Unable to verify — [concern]. To confirm, need [specific file or
> caller].

Do not invent findings.

---

# Lens-specific Non-Issues

Every audit must end with a **Non-Issues (explicitly verified)**
section. For this lens, examples:

- "Reviewed the `Promise.all` on line 88 — wrapped in `try/catch` with
  structured logging, not flagging."
- "Audited the `?? 0` fallback in `getBuildingHeight` — under all
  realistic inputs it returns a value distinct from the pre-fix path's
  output, so it does not re-introduce the bug shape. Not flagging."
