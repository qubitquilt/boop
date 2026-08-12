// readWithRetry: read a file with linear-backoff retry.
//
// The ConfigMap mount uses `..data -> ..2026_…` indirection that
// can be transiently inconsistent right after pod start. This
// helper absorbs that race for every consumer (prompt builder,
// lens loader, walkthrough file reads).
//
// The `reader` argument is the read function — either
// `fs.readFile` (raw) or the rtk adapter's `readFile` (the
// preferred path under QUB-85; the raw path is the fallback when
// the adapter is absent or in "raw" mode).
//
// Failures are surfaced to the caller via `onRetry` so it can log
// progress (the original implementation logged per attempt); the
// function itself stays logger-agnostic. `attempts` defaults to 5
// but is overridable per call (tests pass 1 to skip the backoff
// and exercise the error path immediately).

type Reader = (path: string) => Promise<string>;

export async function readWithRetry(
  path: string,
  reader: Reader,
  {
    attempts = 5,
    onRetry,
  }: { attempts?: number; onRetry?: (n: number, err: unknown) => void } = {},
): Promise<string> {
  let lastErr: unknown;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      return await reader(path);
    } catch (err) {
      lastErr = err;
      if (attempt < attempts) {
        if (onRetry) onRetry(attempt, err);
        await new Promise((r) => setTimeout(r, 1000 * attempt));
      }
    }
  }
  throw lastErr;
}
