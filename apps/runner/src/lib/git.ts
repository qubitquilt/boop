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

import { assertSafeRef, assertSafeSha } from "./security.ts";
import type { Ctx, CleanupRegistry, Deps, FsLike, ExecFileLike } from "../types.ts";

export function createCleanupRegistry(opts: { errlog: (stage: string, msg: string, extra?: Record<string, unknown>) => void }): CleanupRegistry {
  const actions: Array<() => Promise<void> | void> = [];
  return {
    register(fn) {
      actions.push(fn);
    },
    async runAll() {
      const pending = actions.splice(0);
      await Promise.allSettled(
        pending.map(async (fn) => {
          try {
            await fn();
          } catch (err) {
            opts.errlog("cleanup", "cleanup step failed", {
              err: String(err instanceof Error ? err.message : err),
            });
          }
        }),
      );
    },
  };
}

async function writeNetrc(
  token: string,
  fs: FsLike,
  cleanup: CleanupRegistry,
  paths: { netrc: string },
): Promise<void> {
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

async function writeGitconfig(
  fs: FsLike,
  cleanup: CleanupRegistry,
  paths: { netrc: string; gitconfig: string },
): Promise<void> {
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

type CloneRepoDeps = {
  fs: FsLike;
  execFile: ExecFileLike;
  paths: { repoDir: string; netrc: string; gitconfig: string };
  cleanup: CleanupRegistry;
  log: (stage: string, msg: string, extra?: Record<string, unknown>) => void;
  postStatus?: (stage: string) => Promise<void>;
};

export async function cloneRepo(
  token: string,
  ctx: Ctx,
  deps: CloneRepoDeps,
): Promise<void> {
  const { fs, execFile, paths, cleanup, log, postStatus } = deps;
  await fs.rm(paths.repoDir, { recursive: true, force: true });
  await fs.mkdir(paths.repoDir, { recursive: true });
  await writeNetrc(token, fs, cleanup, paths);
  await writeGitconfig(fs, cleanup, paths);

  const gitEnv: NodeJS.ProcessEnv = {
    ...process.env,
    GIT_CONFIG_GLOBAL: paths.gitconfig,
    GIT_CONFIG_NOSYSTEM: "1",
    HOME: ctx.home,
  };

  const baseRef = assertSafeRef("PR_BASE_REF", ctx.prBaseRef);
  const headSha = assertSafeSha("PR_HEAD_SHA", ctx.prHeadSha);

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

  if (postStatus) {
    await postStatus("clone");
  }

  const fetchRefs: string[] = [baseRef, headSha];
  if (ctx.previousHeadSha) {
    fetchRefs.push(assertSafeSha("PR_PREVIOUS_HEAD_SHA", ctx.previousHeadSha));
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
