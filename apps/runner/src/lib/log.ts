// Structured JSON logger.
//
// The runner is a one-shot process; logs go to stdout, are picked up
// by `kubectl logs`, and triaged by humans. The shape is a single JSON
// object per line:
//
//   {"level":"INFO","stage":"clone","msg":"…","pr":"o/r#1","sha":"…", …}
//
// `pr` and `sha` are stamped automatically from a `ctx` (the loaded
// config) so callers don't have to repeat the PR coordinates on every
// line. Extra fields are accepted as a third argument.
//
// `makeLogger(ctx)` returns a bound logger; this is the only thing the
// pipeline needs to import. Tests get the same shape by constructing a
// ctx-shaped object directly.

import type { Ctx } from "../types.ts";

type LogExtra = Record<string, unknown> | undefined;

function emit(
  level: string,
  ctx: Pick<Ctx, "prOwner" | "prRepo" | "prNumber" | "prHeadSha">,
  stage: string,
  msg: string,
  extra: LogExtra,
) {
  const base = {
    level,
    stage,
    msg,
    pr: `${ctx.prOwner}/${ctx.prRepo}#${ctx.prNumber}`,
    sha: ctx.prHeadSha,
  };
  process.stdout.write(JSON.stringify({ ...base, ...(extra || {}) }) + "\n");
}

export function makeLogger(ctx: Pick<Ctx, "prOwner" | "prRepo" | "prNumber" | "prHeadSha">) {
  return {
    log: (stage: string, msg: string, extra?: LogExtra) =>
      emit("INFO", ctx, stage, msg, extra),
    errlog: (stage: string, msg: string, extra?: LogExtra) =>
      emit("ERROR", ctx, stage, msg, extra),
  };
}
