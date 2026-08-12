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
// dedicated `graphql.ts` and keep auth here.

import { Octokit } from "@octokit/rest";
import type { Ctx, Deps, FetchLike, JwtLike, OctokitCtorLike, OctokitLike } from "../../types.ts";

export async function mintInstallationToken(
  appId: string,
  privateKey: string,
  installationId: string,
  deps: { jwt: JwtLike; fetch: FetchLike; fetchImpl?: FetchLike; log?: (stage: string, msg: string, extra?: Record<string, unknown>) => void },
): Promise<string> {
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
  const data = (await res.json()) as { token: string };
  log?.("auth", "minted installation token");
  return data.token;
}

export function makeOctokit(
  installationToken: string,
  deps: { OctokitCtor?: OctokitCtorLike } = {},
): OctokitLike {
  const OctokitCtor: OctokitCtorLike = deps.OctokitCtor || (Octokit as unknown as OctokitCtorLike);
  return new OctokitCtor({ auth: installationToken });
}

async function graphql(
  token: string,
  query: string,
  variables: Record<string, unknown>,
  deps: { fetchImpl?: FetchLike; fetch: FetchLike },
): Promise<unknown> {
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
  const json = (await res.json()) as { data?: unknown; errors?: Array<{ message: string }> };
  if (json.errors && json.errors.length > 0) {
    throw new Error(`graphql: ${json.errors.map((e) => e.message).join("; ")}`);
  }
  return json.data;
}

type ThreadSummary = {
  id: string;
  isResolved: boolean;
  isOutdated: boolean;
  author: string | null;
};

type WireDeps = { fetch: FetchLike; fetchImpl?: FetchLike };

async function fetchAllReviewThreads(
  token: string,
  ctx: Ctx,
  deps: WireDeps,
): Promise<ThreadSummary[]> {
  const threads: ThreadSummary[] = [];
  let cursor: string | null = null;
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
    const data = (await graphql(
      token,
      query,
      {
        owner: ctx.prOwner,
        repo: ctx.prRepo,
        number: Number(ctx.prNumber),
        cursor,
      },
      deps,
    )) as {
      repository?: {
        pullRequest?: {
          reviewThreads?: {
            nodes?: Array<{
              id: string;
              isResolved: boolean;
              isOutdated: boolean;
              comments?: { nodes?: Array<{ databaseId: number; author?: { login: string } }> };
            }>;
            pageInfo?: { hasNextPage: boolean; endCursor: string };
          };
        };
      };
    } | null;
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

type PriorComment = { id: number; nodeId: string };

async function fetchPriorBotIssueCommentIDs(
  token: string,
  ctx: Ctx,
  deps: WireDeps,
): Promise<PriorComment[]> {
  const ids: PriorComment[] = [];
  const headers = {
    Authorization: `bearer ${token}`,
    Accept: "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "boop-runner",
  };
  const { fetchImpl = deps.fetch } = deps;

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
    const arr = (await res.json()) as Array<{
      id: number;
      node_id: string;
      user?: { login?: string };
    }>;
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

async function resolveReviewThread(
  token: string,
  threadId: string,
  deps: WireDeps,
): Promise<boolean> {
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
    const msg = String(err instanceof Error ? err.message : err);
    if (msg.toLowerCase().includes("already")) return false;
    throw err;
  }
}

async function minimizeComment(
  token: string,
  commentNodeId: string,
  deps: WireDeps,
): Promise<boolean> {
  const mutation = `
    mutation($id: ID!) {
      minimizeComment(input: { subjectId: $id, classifier: OUTDATED }) {
        minimizedComment { isMinimized }
      }
    }`;
  await graphql(token, mutation, { id: commentNodeId }, deps);
  return true;
}

export async function cleanupPriorReview(
  token: string,
  ctx: Ctx,
  deps: Pick<Deps, "log" | "errlog" | "fetchImpl"> & { fetch?: FetchLike },
): Promise<{ resolved: number; minimized: number; errors: number }> {
  const { log, errlog } = deps;
  const result = { resolved: 0, minimized: 0, errors: 0 };
  const wireDeps: WireDeps = { fetch: deps.fetch ?? deps.fetchImpl, fetchImpl: deps.fetchImpl };

  const [threads, priors] = await Promise.all([
    fetchAllReviewThreads(token, ctx, wireDeps),
    fetchPriorBotIssueCommentIDs(token, ctx, wireDeps),
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
      if (await resolveReviewThread(token, t.id, wireDeps)) {
        result.resolved++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "resolve failed", {
        thread: t.id,
        err: String(err instanceof Error ? err.message : err),
      });
    }
  }

  log("cleanup", "scanned issue comments", { bot_total: priors.length });
  for (const c of priors) {
    try {
      if (await minimizeComment(token, c.nodeId, wireDeps)) {
        result.minimized++;
      }
    } catch (err) {
      result.errors++;
      errlog("cleanup", "minimize failed", {
        comment: c.id,
        err: String(err instanceof Error ? err.message : err),
      });
    }
  }

  return result;
}
