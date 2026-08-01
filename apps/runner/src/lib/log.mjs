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

function emit(level, ctx, stage, msg, extra) {
  const base = {
    level,
    stage,
    msg,
    pr: `${ctx.prOwner}/${ctx.prRepo}#${ctx.prNumber}`,
    sha: ctx.prHeadSha,
  };
  process.stdout.write(JSON.stringify({ ...base, ...(extra || {}) }) + "\n");
}

export function makeLogger(ctx) {
  return {
    log: (stage, msg, extra) => emit("INFO", ctx, stage, msg, extra),
    errlog: (stage, msg, extra) => emit("ERROR", ctx, stage, msg, extra),
  };
}
