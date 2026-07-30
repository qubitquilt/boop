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

func TestMentionsBot(t *testing.T) {
	cases := []struct {
		body   string
		expect bool
	}{
		{"@BoopPr please review", true},
		{"hey @booppr take a look", true},  // case-insensitive
		{"@BoopPr.", true},                  // followed by punctuation
		{"Multi line\n@BoopPr\nreview", true},
		{"ends with @BoopPr", true},
		{"nothing here", false},
		{"", false},
		{"@BoopPr-bot ignore me", false},    // not a whole-token match
		{"@BoopPrbot hello", false},         // adjacent alnum
		{"@BoopPr2 hi", false},              // adjacent digit
		{"@booppr_bot", false},              // adjacent underscore
	}
	for _, c := range cases {
		got := mentionsBot(c.body)
		if got != c.expect {
			t.Errorf("body=%q: got=%v want=%v", c.body, got, c.expect)
		}
	}
}
