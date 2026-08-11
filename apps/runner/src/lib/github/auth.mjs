// GitHub auth + cleanup (RF-020 split).
//
// The auth/lifecycle surface is everything the runner
// does against the GitHub App to get a token, build the
// Octokit instance, and clean up prior review artifacts.
// Three concerns share this file because they are
// tightly coupled at the wire level: the cleanup path
// uses the same `token` (not the Octokit) so the
// graphql helper can paginate review threads without
// the Octokit's per-request session overhead.
//
//   - mintInstallationToken exchanges the App JWT for a
//     1h installation token.
//   - makeOctokit wraps the Octokit SDK with the token.
//   - graphql posts a query/mutation to the GraphQL API.
//   - cleanupPriorReview resolves outdated review threads
//     and minimizes prior bot issue comments on re-reviews.
//   - The four cleanup helpers (fetchAllReviewThreads,
//     fetchPriorBotIssueCommentIDs, resolveReviewThread,
//     minimizeComment) live here because they only feed
//     cleanupPriorReview.
//
// Future splits: if a second consumer of graphql appears
// (e.g. a QUB-XXX telemetry call that uses GraphQL),
// promote graphql + the four cleanup helpers to a
// dedicated `graphql.mjs` and keep auth here.

import { Octokit } from "@octokit/rest";

// mintInstallationToken exchanges an App JWT for an
// installation token (1h TTL). Used by both status
// updates and the cleanup GraphQL fetches.
export async function mintInstallationToken(appId, privateKey, installationId, deps) {
  const { jwt, fetch, fetchImpl = fetch, log } = deps;
  const now = Math.floor(Date.now() / 1000);
  const appJwt = jwt.sign(
    { iat: now - 30, exp: now + 600, iss: String(appId) },
    privateKey,
    { algorithm: "RS256" },
  );

  const res = await fetchImpl(
    `https://api.github.com/app/installations/${installationId}/access_tokens`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${appJwt}`,
        Accept: "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "boop-runner",
      },
    },
  );
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`mint installation token: ${res.status} ${text}`);
  }
  const data = await res.json();
  return data.token;
}

// makeOctokit is the constructor wrapper the handshake
// stage uses to mint the post-handshake Octokit. The
// runner can inject a fake OctokitCtor via
// deps.OctokitCtor (the test seam in workflow.test.mjs
// uses this to avoid pulling in the real SDK).
export function makeOctokit(installationToken, deps = {}) {
  const OctokitCtor = deps.OctokitCtor || Octokit;
  return new OctokitCtor({ auth: installationToken });
}

// graphql POSTs a query/mutation to api.github.com/graphql
// with the installation token and returns the JSON `data`
// object. Throws on transport errors or top-level
// GraphQL errors.
async function graphql(token, query, variables, deps) {
  const { fetchImpl = fetch } = deps;
  const res = await fetchImpl("https://api.github.com/graphql", {
    method: "POST",
    headers: {
      Authorization: `bearer ${token}`,
      "Content-Type": "application/json",
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": "2022-11-28",
      "User-Agent": "boop-runner",
    },
    body: JSON.stringify({ query, variables }),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`graphql HTTP ${res.status}: ${text.slice(0, 200)}`);
  }
  const json = await res.json();
  if (json.errors && json.errors.length > 0) {
    throw new Error(`graphql: ${json.errors.map((e) => e.message).join("; ")}`);
  }
  return json.data;
}

// paginateThreads walks every review thread on the PR,
// paginating until exhausted. Each returned thread is
// annotated with the original comment's author login
// (case-insensitive match).
async function fetchAllReviewThreads(token, ctx, deps) {
  const threads = [];
  let cursor = null;
  const query = `
    query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
      repository(owner: $owner, name: $repo) {
        pullRequest(number: $number) {
          reviewThreads(first: 100, after: $cursor) {
            pageInfo { hasNextPage endCursor }
            nodes {
              id
              isResolved
              isOutdated
              comments(first: 1) {
                nodes {
                  databaseId
                  author { login }
                }
              }
            }
          }
        }
      }
    }`;
  while (true) {
    const data = await graphql(
      token,
      query,
      {
        owner: ctx.prOwner,
        repo: ctx.prRepo,
        number: Number(ctx.prNumber),
        cursor,
      },
      deps,
    );
    const conn = data?.repository?.pullRequest?.reviewThreads;
    if (!conn) break;
    for (const t of conn.nodes || []) {
      const author = t.comments?.nodes?.[0]?.author?.login;
      threads.push({
        id: t.id,
        isResolved: !!t.isResolved,
        isOutdated: !!t.isOutdated,
        author: author ? author.toLowerCase() : null,
      });
    }
    if (!conn.pageInfo?.hasNextPage) break;
    cursor = conn.pageInfo.endCursor;
  }
  return threads;
}

// fetchPriorBotIssueCommentIDs walks every issue comment
// on the PR via the REST API (GraphQL's
// pullRequest.comments misses some bot comments that
// were posted via the issue-comments API). Returns the
// integer IDs of every comment posted by the bot,
// excluding the current run's status comment.
async function fetchPriorBotIssueCommentIDs(token, ctx, deps) {
  const ids = [];
  const headers = {
    Authorization: `bearer ${token}`,
    Accept: "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "boop-runner",
  };
  const { fetchImpl = fetch } = deps;

  let page = 1;
  while (true) {
    const res = await fetchImpl(
      `https://api.github.com/repos/${ctx.prOwner}/${ctx.prRepo}/issues/${ctx.prNumber}/comments?per_page=100&page=${page}`,
      { headers },
    );
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`list comments HTTP ${res.status}: ${text.slice(0, 200)}`);
    }
    const arr = await res.json();
    if (!Array.isArray(arr) || arr.length === 0) break;
    for (const c of arr) {
      const author = c?.user?.login?.toLowerCase() || "";
      if (ctx.botLogin && author !== ctx.botLogin.toLowerCase()) continue;
      if (ctx.statusCommentId && Number(c.id) === ctx.statusCommentId) continue;
      ids.push({ id: Number(c.id), nodeId: c.node_id });
    }
    if (arr.length < 100) break;
    page++;
  }
  return ids;
}

// resolveReviewThread sends the ResolveReviewThread
// mutation. Returns true on success, false on a
// benign failure (already-resolved, or no-op).
async function resolveReviewThread(token, threadId, deps) {
  const mutation = `
    mutation($id: ID!) {
      resolveReviewThread(input: { threadId: $id }) {
        thread { id isResolved }
      }
    }`;
  try {
    await graphql(token, mutation, { id: threadId }, deps);
    return true;
  } catch (err) {
    // "Already resolved" surfaces as a GraphQL error;
    // we treat that as success because the desired end
    // state is "thread resolved".
    const msg = String(err?.message ?? err);
    if (msg.toLowerCase().includes("already")) return false;
    throw err;
  }
}

// minimizeComment sends the MinimizeComment mutation.
// Returns true on success.
async function minimizeComment(token, commentNodeId, deps) {
  const mutation = `
    mutation($id: ID!) {
      minimizeComment(input: { subjectId: $id, classifier: OUTDATED }) {
        minimizedComment { isMinimized }
      }
    }`;
  await graphql(token, mutation, { id: commentNodeId }, deps);
  return true;
}

// cleanupPriorReview runs on re-reviews only. It:
//   1. Resolves every Boop review thread whose diff line
//      is gone or changed (isOutdated === true) — the
//      author has either fixed the issue or removed the
//      code.
//   2. Minimizes every other prior Boop issue comment
//      (status threads, prior summary comments) so the
//      PR UI is dominated by the active review.
//
// Best-effort. The review already posted — a cleanup
// failure is logged but does not fail the run. The two
// fetches (review threads, issue comments) run in
// parallel; without that, a slow reviewThreads fetch
// would block the minimize pass by ~tens of seconds.
export async function cleanupPriorReview(token, ctx, deps) {
  const { log, errlog } = deps;
  const result = { resolved: 0, minimized: 0, errors: 0 };

  // 1+2 in parallel. Each returns a list we then iterate
  // serially to mutate the threads/comments — the
  // network wait is the slow part and that's what we're
  // collapsing.
  const [threads, priors] = await Promise.all([
    fetchAllReviewThreads(token, ctx, deps),
    fetchPriorBotIssueCommentIDs(token, ctx, deps),
  ]);

  const targets = threads.filter(
    (t) =>
      !t.isResolved &&
      t.isOutdated &&
      ctx.botLogin &&
      t.author === ctx.botLogin.toLowerCase(),
  );
  log("cleanup", "scanned review threads", {
    total: threads.length,
    bot_outdated: targets.length,
  });
  for (const t of targets) {
    try {
      if (await resolveReviewThread(token, t.id, deps)) {
        result.resolved++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "resolve failed", {
        thread: t.id,
        err: String(err?.message ?? err),
      });
    }
  }

  log("cleanup", "scanned issue comments", { bot_total: priors.length });
  for (const c of priors) {
    try {
      if (await minimizeComment(token, c.nodeId, deps)) {
        result.minimized++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "minimize failed", {
        comment: c.id,
        err: String(err?.message ?? err),
      });
    }
  }

  return result;
}
