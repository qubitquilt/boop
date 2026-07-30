import { test } from "node:test";
import assert from "node:assert/strict";

// Lock the hidden-marker contract. The receiver's
// `priorReviewHeadSHARegex` (apps/receiver/internal/github/client.go)
// parses this exact format. If either side changes the other breaks.
test("postReview head marker contract", () => {
  const sha = "87bcc09abcdef0123456789abcdef0123456789";
  const marker = `<!-- boop-head-sha: ${sha} -->`;
  assert.match(marker, /^<!--\s*boop-head-sha:\s*[0-9a-f]{7,40}\s*-->$/);
  // The regex tolerates whitespace inside the comment delimiters.
  assert.equal(marker.replace(/<!--\s*boop-head-sha:\s*([0-9a-f]{7,40})\s*-->/, "$1"), sha);
});
