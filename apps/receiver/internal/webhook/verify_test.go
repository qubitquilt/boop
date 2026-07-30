package webhook

import "testing"

func TestVerifySignature(t *testing.T) {
	const secret = "It's a Secret to Everybody"
	const body = "Hello, World!"
	want := "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if !verifySignature(want, []byte(body), secret) {
		t.Error("expected valid signature to pass")
	}
	if verifySignature("sha256=deadbeef", []byte(body), secret) {
		t.Error("expected invalid signature to fail")
	}
	if verifySignature("nosha256=deadbeef", []byte(body), secret) {
		t.Error("expected non-sha256 prefix to fail")
	}
}

func TestIsReviewableAction(t *testing.T) {
	for _, a := range []string{"opened", "reopened", "synchronize", "ready_for_review"} {
		if !isReviewableAction(a) {
			t.Errorf("expected %q to be reviewable", a)
		}
	}
	for _, a := range []string{"closed", "edited", "assigned", "labeled"} {
		if isReviewableAction(a) {
			t.Errorf("expected %q to NOT be reviewable", a)
		}
	}
}

func TestRequestsReview(t *testing.T) {
	cases := []struct {
		body   string
		expect bool
	}{
		// Bare mentions should NOT trigger — only an explicit request.
		{"@BoopPr hi", false},
		{"hey @booppr take a look", false},
		{"@BoopPr are you awake?", false},
		{"nothing here", false},
		{"", false},
		{"@BoopPr-bot review me", false}, // not a whole-token match
		{"@BoopPrbot review", false},     // adjacent alnum
		{"@BoopPr2 review", false},       // adjacent digit
		{"@booppr_bot review", false},    // adjacent underscore

		// Explicit review requests SHOULD trigger.
		{"@BoopPr review", true},
		{"@BoopPr, review", true},
		{"@BoopPr review please", true},
		{"@BoopPr please review", true},
		{"@BoopPr, please review", true},
		{"@BoopPr to review", true},
		{"@BoopPr can review", true},
		{"@BoopPr can you review this", true},
		{"@BoopPr could you review this", true},
		{"@BoopPr will you review", true},
		{"@BoopPr may you review", true},
		{"@BoopPr re-review", true},
		{"@BoopPr, re-review this", true},
		{"@BoopPr, can you re-review this", true},
		{"Multi line\n@BoopPr review\nthanks", true},
		{"@booppr REVIEW", true}, // case-insensitive
		{"hey @BoopPr please review", true},

		// References to "review" (not requests) must not trigger.
		{"@BoopPr look at this code review carefully", false},
		{"@BoopPr the prior review was great", false},
		{"@BoopPr I left a review on PR #1 yesterday", false},
		{"@BoopPr this needs another look first", false},
	}
	for _, c := range cases {
		got := requestsReview(c.body)
		if got != c.expect {
			t.Errorf("body=%q: got=%v want=%v", c.body, got, c.expect)
		}
	}
}
