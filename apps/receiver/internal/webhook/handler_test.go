package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto/rand"
	"crypto/rsa"
	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"net/url"
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

func TestResolveSDKEnabled(t *testing.T) {
	makeH := func(clusterDefault string) *Handler {
		return &Handler{cfg: Config{OpenRouterSDKDefault: clusterDefault}}
	}
	cases := []struct {
		name           string
		clusterDefault string
		labels         []string
		want           string
	}{
		{"cluster default 0, no label", "0", nil, "0"},
		{"cluster default 0, unrelated label", "0", []string{"bug", "feature"}, "0"},
		{"cluster default 0, sdk label opts in", "0", []string{"boop:openrouter-sdk"}, "1"},
		{"cluster default 0, sdk label case-insensitive", "0", []string{"Boop:OpenRouter-SDK"}, "1"},
		{"cluster default 1, no label", "1", nil, "1"},
		{"cluster default 1, unrelated label", "1", []string{"bug"}, "1"},
		{"cluster default 1, sdk label is redundant", "1", []string{"boop:openrouter-sdk"}, "1"},
		{"cluster default unset, no label", "", nil, "0"},
		{"cluster default unset, sdk label opts in", "", []string{"boop:openrouter-sdk"}, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := makeH(c.clusterDefault)
			if got := h.resolveSDKEnabled(c.labels); got != c.want {
				t.Errorf("resolveSDKEnabled(%q, %v) = %q, want %q", c.clusterDefault, c.labels, got, c.want)
			}
		})
	}
}

func TestBuildJobForwardsOpenRouterSDKEnabled(t *testing.T) {
	job, err := buildJob(templateVars{
		Owner:                "o",
		Repo:                 "r",
		Number:               "1",
		SHA:                  "0123456789abcdef",
		SHA7:                 "0123456",
		BaseRef:              "main",
		Image:                "img",
		InstallationID:       "1",
		OpenRouterSDKEnabled: "1",
	})
	if err != nil {
		t.Fatalf("buildJob: %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	var got string
	for _, e := range env {
		if e.Name == "BOOP_USE_OPENROUTER_SDK" {
			got = e.Value
		}
	}
	if got != "1" {
		t.Errorf("BOOP_USE_OPENROUTER_SDK = %q, want %q", got, "1")
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
	// QUB-102: BackoffLimit is 0. A pod failure surfaces as a
	// failed Job instead of auto-restarting with a fresh pod
	// that would mint a duplicate installation token, PATCH the
	// status comment, and POST duplicate summary + inline
	// comments (the receiver's claimJobSlot dedup is keyed on
	// the first webhook for the head SHA, not the pod index).
	if got := job.Spec.BackoffLimit; got == nil || *got != 0 {
		t.Errorf("BackoffLimit = %v, want 0", got)
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

// TestCurrentJobImage_ReadsConfigMap pins the receiver's
// runtime-JOB_IMAGE-resolution path. The receiver used to bake
// JOB_IMAGE at startup from the boop-config ConfigMap, which
// meant ArgoCD-driven ConfigMap updates didn't reach the Job
// template until the receiver pod was restarted. Today
// (2026-08-01) this caused a real incident: PR #83 bumped the
// runner digest in main, ArgoCD synced the ConfigMap, but the
// next Boop Job pulled the pre-#83 image. The fix is to read
// the ConfigMap on each submitJob call.
func TestCurrentJobImage_ReadsConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{
			"JOB_IMAGE": "ghcr.io/qubitquilt/boop-runner@sha256:f71ad0c70075e10423ba5ec741d826bbd212f36a4dc97d255c5ed51256bbde72",
		},
	}
	client := fake.NewSimpleClientset(cm)
	h := &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			// Stale startup value — currentJobImage must NOT return this
			// when the ConfigMap has a fresh value.
			JobImage: "ghcr.io/qubitquilt/boop-runner@sha256:old-old-old",
		},
		kube: client,
	}
	got, err := h.currentJobImage(context.Background())
	if err != nil {
		t.Fatalf("currentJobImage: %v", err)
	}
	if got != cm.Data["JOB_IMAGE"] {
		t.Errorf("currentJobImage = %q, want %q", got, cm.Data["JOB_IMAGE"])
	}
}

func TestCurrentJobImage_PicksUpLatestDigest(t *testing.T) {
	// Two calls in a row — second call sees an updated ConfigMap.
	// Without re-reading, the second call would return the first
	// call's value (or the env-var snapshot). With re-reading, the
	// receiver picks up the new digest within one submit.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{
			"JOB_IMAGE": "ghcr.io/qubitquilt/boop-runner@sha256:first",
		},
	}
	client := fake.NewSimpleClientset(cm)
	h := &Handler{
		cfg:  Config{TargetNamespace: "dev-tools"},
		kube: client,
	}
	first, err := h.currentJobImage(context.Background())
	if err != nil {
		t.Fatalf("first currentJobImage: %v", err)
	}
	if first != "ghcr.io/qubitquilt/boop-runner@sha256:first" {
		t.Fatalf("first read = %q, want first digest", first)
	}
	// The fake client returns a deep copy from Get, so mutating
	// the original `cm` object does NOT change what subsequent Get
	// calls return. Update the ConfigMap through the fake client
	// to simulate ArgoCD landing a new digest.
	cm.Data["JOB_IMAGE"] = "ghcr.io/qubitquilt/boop-runner@sha256:second"
	updated, err := client.CoreV1().ConfigMaps("dev-tools").Update(
		context.Background(), cm, metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update configmap: %v", err)
	}
	if updated.Data["JOB_IMAGE"] != "ghcr.io/qubitquilt/boop-runner@sha256:second" {
		t.Fatalf("post-update data = %q, want second digest", updated.Data["JOB_IMAGE"])
	}
	second, err := h.currentJobImage(context.Background())
	if err != nil {
		t.Fatalf("second currentJobImage: %v", err)
	}
	if first == second {
		t.Errorf("expected the second read to differ; got both = %q", first)
	}
	if second != "ghcr.io/qubitquilt/boop-runner@sha256:second" {
		t.Errorf("second currentJobImage = %q, want the updated digest", second)
	}
}

func TestCurrentJobImage_FallsBackOnMissingConfigMap(t *testing.T) {
	// No ConfigMap seeded — the read must fail, and the caller
	// (submitJob) falls back to cfg.JobImage (the env-var
	// snapshot). This test pins the read-error contract: missing
	// ConfigMap is NOT a panic, it's a regular error that the
	// caller can recover from.
	client := fake.NewSimpleClientset()
	h := &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			JobImage:        "ghcr.io/qubitquilt/boop-runner@sha256:fallback",
		},
		kube: client,
	}
	_, err := h.currentJobImage(context.Background())
	if err == nil {
		t.Fatal("expected error when ConfigMap is missing, got nil")
	}
	if !strings.Contains(err.Error(), "boop-config") {
		t.Errorf("error %q should reference the ConfigMap name", err)
	}
}

func TestCurrentJobImage_FallsBackOnMissingKey(t *testing.T) {
	// ConfigMap exists but is missing the JOB_IMAGE key. The
	// current code treats this as an error so submitJob falls
	// back to the env-var value rather than silently using an
	// empty string (which would surface as an empty `image:`
	// in the Job template and K8s would reject the Job).
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{
			"LOG_LEVEL": "info",
		},
	}
	client := fake.NewSimpleClientset(cm)
	h := &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			JobImage:        "ghcr.io/qubitquilt/boop-runner@sha256:fallback",
		},
		kube: client,
	}
	_, err := h.currentJobImage(context.Background())
	if err == nil {
		t.Fatal("expected error when JOB_IMAGE key is missing, got nil")
	}
	if !strings.Contains(err.Error(), "JOB_IMAGE") {
		t.Errorf("error %q should reference the missing key", err)
	}
}

// TestResolveJobImageForSubmit_FallbackLogsWarn pins F2: a single
// fallback logs at Warn level (not Error). The Error escalation
// lives in a separate test.
func TestResolveJobImageForSubmit_FallbackLogsWarn(t *testing.T) {
	client := fake.NewSimpleClientset() // no ConfigMap seeded → read fails
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			JobImage:        "ghcr.io/qubitquilt/boop-runner@sha256:fallback",
		},
		kube:   client,
		logger: logger,
	}
	image, source := h.resolveJobImageForSubmit(context.Background())
	if image != h.cfg.JobImage {
		t.Errorf("image = %q, want fallback %q", image, h.cfg.JobImage)
	}
	if source != "fallback" {
		t.Errorf("source = %q, want fallback", source)
	}
	// First failure: Warn.
	if !strings.Contains(logBuf.String(), `"level":"WARN"`) {
		t.Errorf("first fallback should log at WARN, got: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), `"level":"ERROR"`) {
		t.Errorf("first fallback should NOT escalate to ERROR: %s", logBuf.String())
	}
}

// TestResolveJobImageForSubmit_EscalatesAfterThreshold pins F2:
// consecutive fallbacks escalate Warn → Error once the count
// crosses consecutiveFallbackAlertAt. A single successful read in
// between resets the counter.
//
// In production the receiver is one process with one Handler, so
// the counter persists across webhook calls. The test mirrors that
// by constructing ONE Handler and driving multiple calls against
// it — if each call got its own Handler, the counter would always
// start at 0 and the escalation would never fire.
func TestResolveJobImageForSubmit_EscalatesAfterThreshold(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{"JOB_IMAGE": "ghcr.io/qubitquilt/boop-runner@sha256:live"},
	}
	var failConfigMap bool
	var configMapMu sync.Mutex
	client := fake.NewSimpleClientset(cm)
	client.PrependReactor("get", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		configMapMu.Lock()
		defer configMapMu.Unlock()
		if failConfigMap {
			return true, nil, errors.New("simulated API outage")
		}
		return false, nil, nil
	})

	// One Handler shared across all calls so the counter persists.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := &Handler{
		cfg: Config{
			TargetNamespace: "dev-tools",
			JobImage:        "ghcr.io/qubitquilt/boop-runner@sha256:fallback",
		},
		kube:   client,
		logger: logger,
	}

	callOnce := func(label string) {
		logBuf.Reset()
		configMapMu.Lock()
		fail := failConfigMap
		configMapMu.Unlock()
		image, _ := h.resolveJobImageForSubmit(context.Background())
		t.Logf("%s: image=%s fail=%v log=%s", label, image, fail, strings.TrimSpace(logBuf.String()))
	}

	// Three consecutive failures — first two should be WARN, third
	// is the one that crosses the threshold.
	configMapMu.Lock()
	failConfigMap = true
	configMapMu.Unlock()
	callOnce("first failure")
	first := logBuf.String()

	logBuf.Reset()
	callOnce("second failure")
	second := logBuf.String()

	logBuf.Reset()
	callOnce("third failure (crosses threshold)")
	third := logBuf.String()

	if !strings.Contains(first, `"level":"WARN"`) || strings.Contains(first, `"level":"ERROR"`) {
		t.Errorf("first failure should be WARN only, got: %s", first)
	}
	if !strings.Contains(second, `"level":"WARN"`) || strings.Contains(second, `"level":"ERROR"`) {
		t.Errorf("second failure should be WARN only, got: %s", second)
	}
	if !strings.Contains(third, `"level":"ERROR"`) {
		t.Errorf("third failure should escalate to ERROR, got: %s", third)
	}
	// The counter field is logged so dashboards can graph the
	// degradation even before the escalation threshold.
	for i, log := range []string{first, second, third} {
		if !strings.Contains(log, `"consecutive_fallbacks":`) {
			t.Errorf("call %d missing consecutive_fallbacks in log: %s", i+1, log)
		}
	}
	if !strings.Contains(first, `"consecutive_fallbacks":1`) {
		t.Errorf("first failure counter should be 1, got: %s", first)
	}
	if !strings.Contains(third, `"consecutive_fallbacks":3`) {
		t.Errorf("third failure counter should be 3, got: %s", third)
	}

	// Now the API recovers. A single successful read must reset
	// the counter — the next failure should log WARN again, not
	// ERROR.
	configMapMu.Lock()
	failConfigMap = false
	configMapMu.Unlock()
	logBuf.Reset()
	callOnce("recovery read")
	recovery := logBuf.String()
	if !strings.Contains(recovery, `"level":"INFO"`) {
		// Successful read logs nothing — only the failure path
		// emits a log line. Confirm that.
		if recovery != "" {
			t.Errorf("recovery read should produce no log, got: %s", recovery)
		}
	}

	configMapMu.Lock()
	failConfigMap = true
	configMapMu.Unlock()
	logBuf.Reset()
	callOnce("failure after recovery")
	afterRecovery := logBuf.String()
	if !strings.Contains(afterRecovery, `"level":"WARN"`) || strings.Contains(afterRecovery, `"level":"ERROR"`) {
		t.Errorf("post-recovery failure should be WARN (counter reset), got: %s", afterRecovery)
	}
	if !strings.Contains(afterRecovery, `"consecutive_fallbacks":1`) {
		t.Errorf("post-recovery failure should log counter=1, got: %s", afterRecovery)
	}
}

// TestSubmitJob_FallsBackOnConfigMapReadFailure pins F1: even
// when currentJobImage errors, submitJob's Job template uses
// cfg.JobImage (the env-var snapshot). Without this test, a
// future refactor of submitJob that drops the fallback
// assignment would re-introduce the original bug
// (Job created with an empty image field) silently.
func TestSubmitJob_FallsBackOnConfigMapReadFailure(t *testing.T) {
	const fallbackImage = "ghcr.io/qubitquilt/boop-runner@sha256:fallback-from-env"
	const liveImage = "ghcr.io/qubitquilt/boop-runner@sha256:should-NOT-be-used"

	// Seed a ConfigMap that the test would otherwise pick up —
	// proves the fallback path runs even when a fresh digest is
	// available (because the read fails).
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{"JOB_IMAGE": liveImage},
	}
	client := fake.NewSimpleClientset(cm)
	client.PrependReactor("get", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated API outage: ConfigMap get fails")
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := &Handler{
		cfg: Config{
			Port:            "8080",
			WebhookSecret:   "test-secret",
			TargetNamespace: "dev-tools",
			JobImage:        fallbackImage,
		},
		kube:    client,
		logger:  logger,
		dedup:   newDeliveryDedup(4096),
		limiter: nil, // not exercised in this test path
	}

	// submitJob's signature is wide; most args are required for
	// buildJob but not for the image-resolution path we're
	// pinning. reactionCommentID=0 because the pull_request branch
	// doesn't react on the trigger comment.
	w := httptest.NewRecorder()
	h.submitJob(
		context.Background(),
		w,
		"delivery-id-fallback",
		"michaelruelas",
		"family-picnic-platform",
		18,
		"7e895631f15f6ba1a542b5cbf68d7dc8d887de82",
		"main",
		"", // previousHeadSHA
		"pull_request.opened",
		0,          // reactionCommentID
		5153677875, // statusCommentID (validates the Job runs the status thread)
		12345,      // installationID
		1,          // reviewNumber
		nil,        // labels (no per-PR SDK opt-in)
	)

	// Pull the Job the handler created and assert its image.
	jobs, err := client.BatchV1().Jobs("dev-tools").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 Job created, got %d", len(jobs.Items))
	}
	gotImage := jobs.Items[0].Spec.Template.Spec.Containers[0].Image
	if gotImage != fallbackImage {
		t.Errorf("Job image = %q, want fallback %q", gotImage, fallbackImage)
	}
	if gotImage == liveImage {
		t.Errorf("Job image picked up the live ConfigMap value despite the read failure — the read did not actually fail")
	}

	// The fallback log line should appear.
	if !strings.Contains(logBuf.String(), `"msg":"read boop-config for JOB_IMAGE, using startup value"`) {
		t.Errorf("expected fallback log line, got: %s", logBuf.String())
	}
}

// TestSubmitJob_UsesLiveConfigMapWhenAvailable pins the opposite
// half of F1: when the ConfigMap read succeeds, submitJob uses
// the live digest, not the env-var snapshot. Together with
// TestSubmitJob_FallsBackOnConfigMapReadFailure, this test pins
// the full submitJob image-resolution contract.
func TestSubmitJob_UsesLiveConfigMapWhenAvailable(t *testing.T) {
	const liveImage = "ghcr.io/qubitquilt/boop-runner@sha256:live"
	const staleImage = "ghcr.io/qubitquilt/boop-runner@sha256:stale-from-env"

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boop-config",
			Namespace: "dev-tools",
		},
		Data: map[string]string{"JOB_IMAGE": liveImage},
	}
	client := fake.NewSimpleClientset(cm)
	h := &Handler{
		cfg: Config{
			Port:            "8080",
			WebhookSecret:   "test-secret",
			TargetNamespace: "dev-tools",
			JobImage:        staleImage,
		},
		kube:    client,
		logger:  slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo})),
		dedup:   newDeliveryDedup(4096),
		limiter: nil,
	}
	w := httptest.NewRecorder()
	h.submitJob(
		context.Background(),
		w,
		"delivery-id-live",
		"michaelruelas",
		"family-picnic-platform",
		18,
		"7e895631f15f6ba1a542b5cbf68d7dc8d887de82",
		"main",
		"",
		"pull_request.opened",
		0,
		5153677875,
		12345,
		1,
		nil,
	)
	jobs, err := client.BatchV1().Jobs("dev-tools").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 Job created, got %d", len(jobs.Items))
	}
	gotImage := jobs.Items[0].Spec.Template.Spec.Containers[0].Image
	if gotImage != liveImage {
		t.Errorf("Job image = %q, want live %q", gotImage, liveImage)
	}
}

func TestHandleWebhookBoundsSlowGitHubRequest(t *testing.T) {
	// Drives the issue_comment path against a GitHub server that
	// responds normally to /app and the installation-token
	// endpoint, then hangs on PullRequests.Get. The 8s
	// webhookTimeout must kick in and the handler must return a
	// 5xx in well under 8s. A second concurrent webhook must NOT
	// be 429'd, because the limiter token has been released.
	const secret = "test-secret"
	const installationID = int64(1)
	const issueNumber = 42

	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"slug":"BoopPr","name":"BoopPr"}`))
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"v1.test","expires_at":"2099-01-01T00:00:00Z"}`))
		case strings.Contains(r.URL.Path, fmt.Sprintf("/pulls/%d", issueNumber)):
			select {
			case <-hung:
			case <-r.Context().Done():
			}
		default:
			t.Logf("unexpected request path %s", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	origApp := boopgithub.AppInfoURLForTest()
	boopgithub.SetAppInfoURLForTest(srv.URL + "/app")
	t.Cleanup(func() { boopgithub.SetAppInfoURLForTest(origApp) })

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	mgr := boopgithub.NewManager(boopgithub.AppConfig{AppID: 1, PrivateKey: priv})
	// go-github builds absolute URLs against api.github.com.
	// Point the manager's HTTP client at a Transport that
	// rewrites api.github.com -> srv.URL so the test server
	// actually sees the request.
	testURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	mgr.SetBaseHTTPForTest(&http.Client{
		Transport: &rewriteTransport{base: testURL, inner: http.DefaultTransport},
	})

	// The slow PR fetch returns before claimJobSlot runs, so the
	// handler does not need a kube client.
	h := &Handler{
		cfg:      Config{WebhookSecret: secret, BotLogin: "booppr[bot]"},
		logger:   slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		ghClient: mgr,
		dedup:    newDeliveryDedup(4096),
		limiter:  rate.NewLimiter(rate.Limit(20), 40),
	}

	body := []byte(fmt.Sprintf(`{
		"action":"created",
		"installation":{"id":%d},
		"repository":{"name":"repo","owner":{"login":"owner"}},
		"issue":{"number":%d,"pull_request":{"url":"https://api.github.com/repos/owner/repo/pulls/%d"}},
		"comment":{"id":7,"body":"@BoopPr review please"},
		"sender":{"login":"alice"}
	}`, installationID, issueNumber, issueNumber))

	deliver := func(label string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", label)
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("X-Hub-Signature-256", signedPayload(secret, body))
		w := httptest.NewRecorder()
		h.HandleWebhook(w, req)
		return w
	}

	started := time.Now()
	w := deliver("slow-delivery")
	elapsed := time.Since(started)
	if elapsed >= webhookTimeout {
		t.Fatalf("handler took %s, want less than %s", elapsed, webhookTimeout)
	}
	if w.Code < 500 || w.Code >= 600 {
		t.Fatalf("handler returned %d, want 5xx; body=%s", w.Code, w.Body.String())
	}

	// A second concurrent webhook (sequential in this test,
	// since httptest is single-goroutine here, but a real
	// receiver handles them in parallel) must NOT be 429'd.
	// The slow handler held a limiter token while it waited
	// for the PR fetch. If the handler returned a 5xx within
	// 8s — proving the bounded ctx fired — the token has been
	// released by the time this second deliver runs, so the
	// limiter still has tokens to spare. A regression that
	// removed the bounded ctx would either run the first call
	// for ~15s (the GitHub per-call timeout) and burn more
	// tokens or — without any per-call timeout — block until
	// the test server hung, exhausting the burst. The 8s
	// bound is the regression guard; the second call's
	// timing here is belt-and-braces.
	secondStart := time.Now()
	second := deliver("concurrent-delivery")
	secondElapsed := time.Since(secondStart)
	if second.Code == http.StatusTooManyRequests {
		t.Fatal("concurrent webhook was rate limited; the slow handler held a limiter token too long")
	}
	if secondElapsed >= webhookTimeout {
		t.Fatalf("concurrent webhook took %s, want less than %s", secondElapsed, webhookTimeout)
	}
}

func signedPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// rewriteTransport is an http.RoundTripper that rewrites every
// request whose host is api.github.com to point at a test
// server, then delegates to the inner RoundTripper. Used by
// TestHandleWebhookBoundsSlowGitHubRequest to redirect go-github
// (which builds absolute URLs against api.github.com) at a
// httptest.Server.
type rewriteTransport struct {
	base  *url.URL
	inner http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.github.com" {
		clone := *req.URL
		clone.Scheme = t.base.Scheme
		clone.Host = t.base.Host
		req = req.Clone(req.Context())
		req.URL = &clone
	}
	return t.inner.RoundTrip(req)
}
