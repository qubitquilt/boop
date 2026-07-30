import { test } from "node:test";
import assert from "node:assert/strict";
import { reviewHeader } from "./review-header.mjs";

// Keep lockstep with apps/receiver/internal/github/review_header_test.go
test("reviewHeader matches receiver ReviewSummaryHeader fixtures", () => {
  assert.equal(reviewHeader(0), "## 🐾 Boop's review");
  assert.equal(reviewHeader(1), "## 🐾 Boop's review");
  assert.equal(reviewHeader(undefined), "## 🐾 Boop's review");
  assert.equal(reviewHeader(null), "## 🐾 Boop's review");
  assert.equal(reviewHeader(2), "## 🐾 Boop's re-review #2");
  assert.equal(reviewHeader(3), "## 🐾 Boop's re-review #3");
  assert.equal(reviewHeader(10), "## 🐾 Boop's re-review #10");
});
