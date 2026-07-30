package webhook

import (
	"strings"
	"testing"
)

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

func TestRenderJobTemplate(t *testing.T) {
	job, err := renderJobTemplate(jobTemplate, templateVars{
		Owner:   "michaelruelas",
		Repo:    "homelab-infra",
		Number:  "42",
		SHA:     "abc1234567890",
		SHA7:    "abc1234",
		BaseRef: "main",
		Image:   "ghcr.io/michaelruelas/boop-runner:dev",
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
	if !strings.Contains(job.Spec.Template.Spec.NodeSelector["kubernetes.io/os"], "linux") && job.Spec.Template.Spec.NodeSelector != nil {
		// no-op, just here to silence unused import warning in case
	}
}
