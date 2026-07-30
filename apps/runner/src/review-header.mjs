// Summary comment H2 for a Boop review run.
// Must stay identical to github.ReviewSummaryHeader in the receiver
// (apps/receiver/internal/github/client.go). Tests pin both sides.

export function reviewHeader(n) {
  if (!n || n <= 1) return "## 🐾 Boop's review";
  return `## 🐾 Boop's re-review #${n}`;
}
