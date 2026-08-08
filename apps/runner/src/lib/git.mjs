// Git plumbing: netrc, gitconfig, shallow clone.
//
// `cloneRepo` is the side-effecting workhorse. It uses a netrc +
// gitconfig pair (both 0600 and unlinked in cleanup) instead of an
// inline token URL so the installation token never appears in argv,
// `.git/config`, or `ps aux`.
//
// All `git` invocations are scoped to this Job's throwaway config via
// `GIT_CONFIG_GLOBAL` / `GIT_CONFIG_NOSYSTEM` so the host's
// `/etc/gitconfig` cannot pull in a system-wide credential helper.
// `--` is the canonical "stop parsing options" marker; everything
// after it is a positional ref. The
// refnames have already been regex-validated by the caller.

// CleanupRegistry is the small helper every other module uses to
// register best-effort cleanup hooks (unlink a file, revoke a netrc).
// The actions are stored in registration order; runAll runs them in
// parallel and swallows individual failures so one stuck unlink does
// not block the rest. Tests can construct a registry without touching
// real files.
export function createCleanupRegistry({ errlog }) {
  const actions = [];
  return {
    register(fn) {
      actions.push(fn);
    },
    async runAll() {
      // splice(0) returns a copy and clears the list atomically, so a
      // cleanup hook that itself registers another hook doesn't
      // re-enter this run.
      const pending = actions.splice(0);
      await Promise.allSettled(
        pending.map(async (fn) => {
          try {
            await fn();
          } catch (err) {
            errlog("cleanup", "cleanup step failed", {
              err: String(err?.message ?? err),
            });
          }
        }),
      );
    },
  };
}

// writeNetrc drops a short-lived .netrc with the installation token.
// The file is created with mode 0600 and unlinked in the cleanup
// phase. Used in place of `https://x-access-token:TOKEN@github.com/...`
// so the token never appears in argv or in the resulting .git/config.
async function writeNetrc(token, fs, cleanup, paths) {
  // Git's netrc implementation accepts the standard three-field form:
  // machine, login, password. We scope it to github.com so a future
  // clone of any other host wouldn't accidentally leak.
  const body = `machine github.com\nlogin x-access-token\npassword ${token}\n`;
  await fs.writeFile(paths.netrc, body, { mode: 0o600 });
  cleanup.register(async () => {
    try {
      await fs.unlink(paths.netrc);
    } catch {
      // already gone
    }
  });
}

// writeGitconfig sets `credential.useHttpPath=false` and points the
// `credential.helper` to a `store` file we control, so the token can
// be unlinked in cleanup. Without this, embedding the token in the
// clone URL is the only way to authenticate, which leaves the token
// visible in .git/config and `ps aux`.
async function writeGitconfig(fs, cleanup, paths) {
  // The store helper is the only credential helper in the chain — no
  // `-c credential.helper=` override will suppress it. Combined with
  // GIT_CONFIG_GLOBAL / GIT_CONFIG_NOSYSTEM we get a fully scoped,
  // throwaway auth chain. The config file is consumed only
  // for this Job and unlinked in cleanup.
  const body = [
    "[credential]",
    `    helper = store --file=${paths.netrc}`,
    "[safe]",
    "    directory = *",
    "",
  ].join("\n");
  await fs.writeFile(paths.gitconfig, body, { mode: 0o600 });
  cleanup.register(async () => {
    try {
      await fs.unlink(paths.gitconfig);
    } catch {
      // already gone
    }
  });
}

// cloneRepo performs the throwaway auth setup and the three git
// invocations (clone, fetch, checkout). baseRef and headSha are
// already validated by the caller; we re-assert here so a future
// refactor that calls cloneRepo with a fresh ctx doesn't have to
// remember.
//
// deps:
//   fs         — node:fs/promises
//   execFile   — promisified execFile
//   paths      — { netrc, gitconfig, repoDir }
//   log        — logger
//   postStatus — async (stage) => void; called with "clone" after the
//                clone completes so the PR status timeline surfaces
//                the 🥎 fetched stage. Best-effort: callers should
//                swallow postStatus errors (the runner's postStatus
//                already does this internally).
export async function cloneRepo(ctx, deps) {
  const { fs, execFile, paths, cleanup, log, postStatus } = deps;
  await fs.rm(paths.repoDir, { recursive: true, force: true });
  await fs.mkdir(paths.repoDir, { recursive: true });
  await writeNetrc(ctx.installationToken, fs, cleanup, paths);
  await writeGitconfig(fs, cleanup, paths);

  // All git invocations use GIT_CONFIG_GLOBAL / GIT_CONFIG_NOSYSTEM to
  // scope auth to this Job's throwaway config, so the host's
  // /etc/gitconfig cannot pull in a system-wide credential helper.
  // `--` is the canonical "stop parsing options" marker; everything
  // after it is a positional ref.
  const gitEnv = {
    ...process.env,
    GIT_CONFIG_GLOBAL: paths.gitconfig,
    GIT_CONFIG_NOSYSTEM: "1",
    HOME: ctx.home,
  };

  const baseRef = assertSafeRefLocal("PR_BASE_REF", ctx.prBaseRef);
  const headSha = assertSafeShaLocal("PR_HEAD_SHA", ctx.prHeadSha);

  log("clone", "cloning repo", { dir: paths.repoDir });
  await execFile(
    "git",
    [
      "-c", "protocol.version=2",
      "clone",
      "--depth", "50",
      "--no-single-branch",
      `https://github.com/${ctx.prOwner}/${ctx.prRepo}.git`,
      paths.repoDir,
    ],
    { env: gitEnv, timeout: 5 * 60 * 1000 },
  );

  // Surface the 🥎 fetched stage in the PR status timeline. Done
  // after the clone (so the user can see clone succeeded) and
  // before the fetch+checkout (so a fetch failure doesn't drop the
  // "clone done" signal). postStatus itself swallows errors; if the
  // status comment API is broken the clone still completes.
  if (postStatus) {
    await postStatus("clone");
  }

  // Fetch every ref the prompt or the LLM might want to `git diff`
  // against. On a re-review with a known prior head, that ref must
  // be present locally so the LLM can run `git diff <prior>...<head>`.
  // The `fetchRefs` list is built from PR-controlled values that are
  // already regex-validated, so a malicious refname cannot smuggle
  // a `--upload-pack=evil` argument.
  const fetchRefs = [baseRef, headSha];
  if (ctx.previousHeadSha) {
    fetchRefs.push(assertSafeShaLocal("PR_PREVIOUS_HEAD_SHA", ctx.previousHeadSha));
  }
  await execFile(
    "git",
    [
      "fetch",
      "--depth", "200",
      "origin",
      "--",
      ...fetchRefs,
    ],
    { env: gitEnv, cwd: paths.repoDir, timeout: 5 * 60 * 1000 },
  );
  await execFile(
    "git",
    [
      "checkout",
      headSha,
    ],
    { env: gitEnv, cwd: paths.repoDir },
  );
}

// Local re-validators that wrap the security module. The caller has
// already run the public asserts on the ctx, but we re-check here so
// a future caller that skips that step fails loud instead of passing
// an unvalidated refname to `git`.
import { assertSafeRef, assertSafeSha } from "./security.mjs";

const assertSafeRefLocal = (n, v) => assertSafeRef(n, v);
const assertSafeShaLocal = (n, v) => assertSafeSha(n, v);
