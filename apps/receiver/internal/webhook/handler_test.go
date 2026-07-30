package webhook

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestRenderStatusBody locks the initial status comment template the
// receiver posts before the runner takes over. The runner's PATCH path
// depends on the rendered body containing `<!-- boop-timeline -->`
// exactly once and on the (review) / (re-review #N) review label
// staying inside the parentheses. Voice changes are welcome, but the
// separator, the commit line, and the label placement must hold.
func TestRenderStatusBody(t *testing.T) {
	const sha = "87bcc09abcdef0123456789abcdef0123456789"
	body := renderStatusBody(StatusInitial, sha, "", 1)
	if !strings.HasPrefix(body, "🐾 **Boop's on the case!** (review)") {
		t.Errorf("initial header = %q, want 🐾 **Boop's on the case!** (review) prefix", body)
	}
	if !strings.Contains(body, "Last commit: `87bcc09`") {
		t.Errorf("initial body missing short SHA line: %q", body)
	}
	if strings.Count(body, statusTimelineSep) != 1 {
		t.Errorf("initial body must contain %s exactly once, got %d matches: %q", statusTimelineSep, strings.Count(body, statusTimelineSep), body)
	}
	// Re-review labels land in the same parentheses slot.
	body2 := renderStatusBody(StatusInitial, sha, "alice", 3)
	if !strings.HasPrefix(body2, "🐾 **Boop's on the case!** (re-review #3)") {
		t.Errorf("re-review header = %q, want re-review #3 prefix", body2)
	}
	if !strings.Contains(body2, "Triggered by @alice") {
		t.Errorf("re-review body missing trigger attribution: %q", body2)
	}
	// Done and Failed stages keep the brand mascot and the merge signal.
	done := renderStatusBody(StatusDone, sha, "", 2)
	if !strings.Contains(done, "💤 **Boop napped") || !strings.Contains(done, "See the comment below") {
		t.Errorf("done body off-brand: %q", done)
	}
	failed := renderStatusBody(StatusFailed, sha, "", 1)
	if !strings.Contains(failed, "🔄 **Boop chased his tail") {
		t.Errorf("failed body off-brand: %q", failed)
	}
}

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

func TestBuildJob(t *testing.T) {
	job, err := buildJob(templateVars{
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
		BotLogin:        "booppr[bot]",
	})
	if err != nil {
		t.Fatalf("buildJob: %v", err)
	}
	if job.Name != "boop-michaelruelas-homelab-infra-42-abc1234" {
		t.Errorf("job.Name = %q", job.Name)
	}
	if got := job.Namespace; got != "dev-tools" {
		t.Errorf("job.Namespace = %q, want dev-tools", got)
	}
	cont := job.Spec.Template.Spec.Containers[0]
	if cont.Image != "ghcr.io/michaelruelas/boop-runner:dev" {
		t.Errorf("image = %q", cont.Image)
	}
	if got := cont.ImagePullPolicy; got != "IfNotPresent" {
		t.Errorf("imagePullPolicy = %q, want IfNotPresent", got)
	}
	if got := job.Spec.TTLSecondsAfterFinished; got == nil || *got != 3600 {
		t.Errorf("TTLSecondsAfterFinished = %v", got)
	}
	if got := job.Spec.ActiveDeadlineSeconds; got == nil || *got != 1800 {
		t.Errorf("ActiveDeadlineSeconds = %v, want 1800", got)
	}
	if got := job.Spec.BackoffLimit; got == nil || *got != 1 {
		t.Errorf("BackoffLimit = %v, want 1", got)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never", got)
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != "boop-job" {
		t.Errorf("serviceAccountName = %q, want boop-job", got)
	}

	// H2: read-only root FS + capability drop + seccomp must be
	// applied so a code-exec bug in the runner cannot persist.
	if cont.SecurityContext == nil {
		t.Fatal("container securityContext is nil")
	}
	if cont.SecurityContext.ReadOnlyRootFilesystem == nil || !*cont.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("readOnlyRootFilesystem = %v, want true", cont.SecurityContext.ReadOnlyRootFilesystem)
	}
	if cont.SecurityContext.AllowPrivilegeEscalation == nil || *cont.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("allowPrivilegeEscalation = %v, want false", cont.SecurityContext.AllowPrivilegeEscalation)
	}
	if cont.SecurityContext.SeccompProfile == nil || cont.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("container seccompProfile = %+v, want RuntimeDefault", cont.SecurityContext.SeccompProfile)
	}
	if cont.SecurityContext.Capabilities == nil {
		t.Fatal("container capabilities is nil")
	}
	drops := map[corev1.Capability]bool{}
	for _, c := range cont.SecurityContext.Capabilities.Drop {
		drops[c] = true
	}
	if !drops["ALL"] {
		t.Errorf("capabilities.drop must include ALL, got %v", cont.SecurityContext.Capabilities.Drop)
	}
	if podSC := job.Spec.Template.Spec.SecurityContext; podSC == nil || podSC.SeccompProfile == nil || podSC.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("pod seccompProfile = %+v, want RuntimeDefault", podSC)
	}
	if podSC := job.Spec.Template.Spec.SecurityContext; podSC == nil || podSC.RunAsNonRoot == nil || !*podSC.RunAsNonRoot {
		t.Errorf("pod runAsNonRoot must be true")
	}

	// /work and /tmp must be mounted as emptyDir so the runner has
	// a writable scratch space with readOnlyRootFilesystem=true.
	mounts := map[string]bool{}
	for _, m := range cont.VolumeMounts {
		mounts[m.MountPath] = true
	}
	if !mounts["/work"] {
		t.Errorf("/work must be mounted (emptyDir)")
	}
	if !mounts["/tmp"] {
		t.Errorf("/tmp must be mounted (emptyDir)")
	}
	if !mounts["/secrets/github-app-private-key"] {
		t.Errorf("private-key mount missing")
	}
	if !mounts["/secrets/openrouter-api-key"] {
		t.Errorf("openrouter-key mount missing")
	}

	// H6/L8: GITHUB_APP_PRIVATE_KEY and OPENROUTER_API_KEY must NOT
	// be exposed as env. A prompt-injected LLM can read them via
	// /proc/self/environ. The runner now reads the mounted file.
	for _, e := range cont.Env {
		if e.Name == "GITHUB_APP_PRIVATE_KEY" {
			t.Errorf("GITHUB_APP_PRIVATE_KEY must NOT be in container env")
		}
		if e.Name == "OPENROUTER_API_KEY" {
			t.Errorf("OPENROUTER_API_KEY must NOT be in container env")
		}
	}

	// BOOP_REVIEW_NUMBER, GITHUB_APP_INSTALLATION_ID,
	// PR_PREVIOUS_HEAD_SHA, and BOOP_BOT_LOGIN must be wired into
	// the container env. The first three label this run and diff
	// only the delta; the last lets the runner identify its own
	// prior comments on a re-review and resolve/minimize them.
	var gotReview, gotInstall, gotPrev, gotLogin string
	for _, e := range cont.Env {
		switch e.Name {
		case "BOOP_REVIEW_NUMBER":
			gotReview = e.Value
		case "GITHUB_APP_INSTALLATION_ID":
			gotInstall = e.Value
		case "PR_PREVIOUS_HEAD_SHA":
			gotPrev = e.Value
		case "BOOP_BOT_LOGIN":
			gotLogin = e.Value
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
	if gotLogin != "booppr[bot]" {
		t.Errorf("BOOP_BOT_LOGIN = %q, want booppr[bot]", gotLogin)
	}
	// Same value must be on the Job annotation so it's discoverable
	// for debugging without grepping the env.
	if got := job.Annotations["boop/previous-head-sha"]; got != "20cd521abcdef0123456789abcdef0123456789" {
		t.Errorf("boop/previous-head-sha annotation = %q, want prior SHA", got)
	}
}

func TestBuildJob_RejectsUnsafeBaseRef(t *testing.T) {
	// C1: a base ref with YAML-significant characters must be
	// rejected at the boundary, not silently rendered into the
	// Job spec. Each case picks a payload that would have broken
	// the old strings.ReplaceAll + yaml.Unmarshal pipeline.
	cases := []struct {
		name string
		ref  string
	}{
		{"empty", ""},
		{"yaml-string-break", `main" - env: [X]`},
		{"yaml-newline", "main\n  - evil: payload"},
		{"yaml-flow-mapping", "main} extra"},
		{"shell-arg", "--upload-pack=evil"},
		{"starts-with-slash", "/main"},
		{"ends-with-slash", "main/"},
		{"double-dot", "main..branch"},
		{"ends-with-dotlock", "main.lock"},
		{"control-chars", "main\x00branch"},
		{"space", "main branch"},
		{"colon", "main:ref"},
		{"asterisk", "main*"},
		{"backtick", "main`evil`"},
		{"too-long", "a" + strings.Repeat("b", 300)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildJob(templateVars{
				Owner:          "o",
				Repo:           "r",
				Number:         "1",
				SHA:            "abc1234567890",
				SHA7:           "abc1234",
				BaseRef:        c.ref,
				Image:          "ghcr.io/example/boop-runner:stable",
				ReviewNumber:   "1",
				InstallationID: "1234",
				BotLogin:       "booppr[bot]",
			})
			if err == nil {
				t.Errorf("buildJob accepted unsafe base ref %q", c.ref)
			}
		})
	}
}

func TestBuildJob_RejectsBadInstallationID(t *testing.T) {
	// M2: the formatted InstallationID must be a positive integer
	// before it lands in the Job. parseInstallationID already
	// gates the header; buildJob gates the value that ends up in
	// the container env.
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"zero", "0"},
		{"negative", "-1"},
		{"non-numeric", "not-a-number"},
		{"hex", "0x10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildJob(templateVars{
				Owner:          "o",
				Repo:           "r",
				Number:         "1",
				SHA:            "abc1234567890",
				SHA7:           "abc1234",
				BaseRef:        "main",
				Image:          "ghcr.io/example/boop-runner:stable",
				ReviewNumber:   "1",
				InstallationID: c.id,
				BotLogin:       "booppr[bot]",
			})
			if err == nil {
				t.Errorf("buildJob accepted installation id %q", c.id)
			}
		})
	}
}

func TestBuildJob_RejectsBadPreviousHeadSHA(t *testing.T) {
	cases := []string{
		"not-a-sha",
		"20cd521abcdef0123456789abcdef01234567890z", // non-hex
		"../etc/passwd",
		"main",
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			_, err := buildJob(templateVars{
				Owner:           "o",
				Repo:            "r",
				Number:          "1",
				SHA:             "abc1234567890",
				SHA7:            "abc1234",
				BaseRef:         "main",
				PreviousHeadSHA: ref,
				Image:           "ghcr.io/example/boop-runner:stable",
				ReviewNumber:    "1",
				InstallationID:  "1234",
				BotLogin:        "booppr[bot]",
			})
			if err == nil {
				t.Errorf("buildJob accepted previous head SHA %q", ref)
			}
		})
	}
}
