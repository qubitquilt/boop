package github

import "testing"

// Headers the runner is expected to emit. Mirrored by
// apps/runner/src/review-header.test.mjs — change both if you change the format.
func TestReviewSummaryHeader(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "## 🐾 Boop's review"},
		{1, "## 🐾 Boop's review"},
		{2, "## 🐾 Boop's re-review #2"},
		{3, "## 🐾 Boop's re-review #3"},
		{10, "## 🐾 Boop's re-review #10"},
	}
	for _, c := range cases {
		if got := ReviewSummaryHeader(c.n); got != c.want {
			t.Errorf("ReviewSummaryHeader(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestIsBoopReviewSummary(t *testing.T) {
	cases := []struct {
		body   string
		expect bool
	}{
		// Exact headers the runner posts today.
		{ReviewSummaryHeader(1) + "\n\nbody", true},
		{ReviewSummaryHeader(2) + "\n\nbody", true},
		{ReviewSummaryHeader(10) + "\n\nbody", true},
		// Historical / tolerant forms still counted as prior reviews.
		{"## 🐾 Boop's re-review\n\nold form without number", true},
		{"##  🐾  Boop's review\n\nextra spaces", true},
		// Non-summaries must not count (status threads, quotes, wrong level).
		{"🐾 **Boop's on the case!** (re-review #2)", false},
		{"> ## 🐾 Boop's review\n\nquoted", false},
		{"# 🐾 Boop's review", false},
		{"## 🐾 Boop’s review", false}, // curly apostrophe
		{"random comment", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsBoopReviewSummary(c.body); got != c.expect {
			t.Errorf("IsBoopReviewSummary(%q): got=%v want=%v", c.body, got, c.expect)
		}
	}
}

func TestExtractPriorReviewSHA(t *testing.T) {
	const fullSHA = "87bcc09abcdef0123456789abcdef0123456789" // 40 hex chars
	// Full 40-char SHA (what the runner emits today).
	body := ReviewSummaryHeader(2) + "\n\nbody\n\n" +
		"<sub>Posted by BoopPr · PR `87bcc09` · review #2 · good boy powered</sub>\n" +
		"<!-- boop-head-sha: " + fullSHA + " -->"
	if got := extractPriorReviewSHA(body); got != fullSHA {
		t.Errorf("full-SHA extract = %q, want %q", got, fullSHA)
	}
	// Short (7-char) SHA still extracts — older clients may emit this.
	bodyShort := "## 🐾 Boop's review\n\nbody\n<!-- boop-head-sha: 87bcc09 -->"
	if got := extractPriorReviewSHA(bodyShort); got != "87bcc09" {
		t.Errorf("short-SHA extract = %q, want 87bcc09", got)
	}
	// No marker present — older summaries, before the marker existed.
	if got := extractPriorReviewSHA("## 🐾 Boop's review\n\nno marker here"); got != "" {
		t.Errorf("absent-marker extract = %q, want empty", got)
	}
	// Malformed marker (non-hex chars between the colons).
	if got := extractPriorReviewSHA("## 🐾 Boop's review\n\n<!-- boop-head-sha: not-a-sha -->"); got != "" {
		t.Errorf("malformed-marker extract = %q, want empty", got)
	}
}
