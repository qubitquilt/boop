package webhook

import (
	"strings"
	"testing"
)

func TestParseInstallationID(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"12345", 12345, false},
		{"1", 1, false},
		{"", 0, true},
		{"abc", 0, true},
		{"0", 0, true},
		{"-1", 0, true},
	}
	for _, c := range cases {
		got, err := parseInstallationID(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseInstallationID(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseInstallationID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestResolveInstallationID(t *testing.T) {
	cases := []struct {
		name      string
		headerVal string
		body      string
		want      int64
		wantErr   bool
	}{
		{
			name:      "header wins when valid",
			headerVal: "42",
			body:      `{"installation":{"id":999}}`,
			want:      42,
		},
		{
			name:      "falls back to payload when header missing",
			headerVal: "",
			body:      `{"installation":{"id":7777}}`,
			want:      7777,
		},
		{
			name:      "falls back to payload when header is invalid",
			headerVal: "not-a-number",
			body:      `{"installation":{"id":1234}}`,
			want:      1234,
		},
		{
			name:      "errors when payload has no installation",
			headerVal: "",
			body:      `{"action":"opened"}`,
			wantErr:   true,
		},
		{
			name:      "errors when body is malformed and header missing",
			headerVal: "",
			body:      `{`,
			wantErr:   true,
		},
		{
			name:      "errors when both are missing",
			headerVal: "",
			body:      `{}`,
			wantErr:   true,
		},
		{
			name:      "errors when installation.id is zero",
			headerVal: "",
			body:      `{"installation":{"id":0}}`,
			wantErr:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveInstallationID(c.headerVal, []byte(c.body))
			if (err != nil) != c.wantErr {
				t.Errorf("resolveInstallationID(%q, %q) err = %v, wantErr = %v", c.headerVal, c.body, err, c.wantErr)
				return
			}
			if !c.wantErr && got != c.want {
				t.Errorf("resolveInstallationID(%q, %q) = %d, want %d", c.headerVal, c.body, got, c.want)
			}
		})
	}
}

func TestBuildJobName(t *testing.T) {
	got := buildJobName("michaelruelas", "homelab-infra", 42, "abc1234567890def")
	want := "boop-michaelruelas-homelab-infra-42-abc1234"
	if got != want {
		t.Errorf("buildJobName = %q, want %q", got, want)
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("abc1234567890"); got != "abc1234" {
		t.Errorf("shortSHA long = %q, want abc1234", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q, want abc", got)
	}
}

func TestDuplicateReviewReply(t *testing.T) {
	active := duplicateReviewReply("active", "abc1234567890def")
	if !strings.Contains(active, "`abc1234`") || !strings.Contains(active, "Already on it") {
		t.Errorf("active reply unexpected: %q", active)
	}
	done := duplicateReviewReply("succeeded", "abc1234567890def")
	if !strings.Contains(done, "`abc1234`") || !strings.Contains(done, "Already sniffed") {
		t.Errorf("succeeded reply unexpected: %q", done)
	}
	if got := duplicateReviewReply("error", "abc"); got != "" {
		t.Errorf("error reply = %q, want empty", got)
	}
}

func TestRenderJobTemplate(t *testing.T) {
	job, err := renderJobTemplate(jobTemplate, templateVars{
		Owner:           "michaelruelas",
		Repo:            "homelab-infra",
		Number:          "42",
		SHA:             "abc1234567890",
		SHA7:            "abc1234",
		BaseRef:         "main",
		PreviousHeadSHA: "20cd521abcdef0123456789abcdef0123456789", // 40 hex chars
		Image:           "ghcr.io/michaelruelas/boop-runner:dev",
		ReviewNumber:    "2",
		InstallationID:  "987654",
	})
	if err != nil {
		t.Fatalf("renderJobTemplate: %v", err)
	}
	if job.Name != "boop-michaelruelas-homelab-infra-42-abc1234" {
		t.Errorf("job.Name = %q", job.Name)
	}
	if job.Spec.Template.Spec.Containers[0].Image != "ghcr.io/michaelruelas/boop-runner:dev" {
		t.Errorf("image = %q", job.Spec.Template.Spec.Containers[0].Image)
	}
	if got := job.Spec.TTLSecondsAfterFinished; got == nil || *got != 3600 {
		t.Errorf("TTLSecondsAfterFinished = %v", got)
	}

	// BOOP_REVIEW_NUMBER, GITHUB_APP_INSTALLATION_ID, and
	// PR_PREVIOUS_HEAD_SHA must be wired into the container env so
	// the runner can label this run and diff only the delta from
	// the previously reviewed commit.
	var gotReview, gotInstall, gotPrev string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "BOOP_REVIEW_NUMBER" {
			gotReview = e.Value
		}
		if e.Name == "GITHUB_APP_INSTALLATION_ID" {
			gotInstall = e.Value
		}
		if e.Name == "PR_PREVIOUS_HEAD_SHA" {
			gotPrev = e.Value
		}
	}
	if gotReview != "2" {
		t.Errorf("BOOP_REVIEW_NUMBER = %q, want 2", gotReview)
	}
	if gotInstall != "987654" {
		t.Errorf("GITHUB_APP_INSTALLATION_ID = %q, want 987654", gotInstall)
	}
	if gotPrev != "20cd521abcdef0123456789abcdef0123456789" {
		t.Errorf("PR_PREVIOUS_HEAD_SHA = %q, want prior SHA", gotPrev)
	}
	// Same value must be on the Job annotation so it's discoverable
	// for debugging without grepping the env.
	if got := job.Annotations["boop/previous-head-sha"]; got != "20cd521abcdef0123456789abcdef0123456789" {
		t.Errorf("boop/previous-head-sha annotation = %q, want prior SHA", got)
	}
}
