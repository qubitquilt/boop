package webhook

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// baseRefRegex permits the characters a legitimate git refname may
// contain: ASCII alnum, dot, underscore, slash, and hyphen. Anything
// outside this set is rejected before it can reach the Job spec. The
// regex is a defense-in-depth check; the Job is now built with
// typed K8s objects (see buildJob) so a hostile ref can no longer
// break out of the YAML structure, but rejecting bad input at the
// boundary also keeps the Job name and labels clean.
var baseRefRegex = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

var hexSHAFullRegex = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

const (
	// Namespace every review Job is created in. Centralised so the
	// buildJob function and the kube client agree without a
	// cross-package constant.
	jobNamespace = "dev-tools"

	// Volume and mount names. Kept short and stable so a kubectl
	// describe on a failed Job reads naturally.
	runnerConfigVolume        = "runner-config"
	privateKeyVolume          = "github-app-private-key"
	openrouterKeyVolume       = "openrouter-api-key"
	workVolume                = "work"
	tmpVolume                 = "tmp"
	privateKeyMountPath       = "/secrets/github-app-private-key"
	openrouterKeyMountPath    = "/secrets/openrouter-api-key"
	runnerConfigMountPath     = "/home/opencode/.config/opencode"
	workMountPath             = "/work"
	tmpMountPath              = "/tmp"
	boopRunnerConfigConfigMap = "boop-runner-config"
	boopSecretsName           = "boop-secrets"
	boopJobServiceAccount     = "boop-job"

	// Mode for mounted secret files. 0400 = read-only for owner, no
	// access for group/other. The pod runs as uid 1000 (see
	// buildJob), so the secret file is readable by the runner and
	// unreadable by any other uid that might land in the pod via
	// a co-located process or a future sidecar.
	secretFileMode int32 = 0o400

	// Job timing knobs. The runner has a 25-min hard kill on
	// opencode (see OPENCODE_TIMEOUT_MS in apps/runner/src/index.mjs),
	// leaving 5 min of headroom against the 30-min active deadline.
	jobTTLSeconds         = 3600
	jobBackoffLimit       = 1
	jobActiveDeadlineSecs = 1800

	// Resource requests and limits. Mirrored from the old
	// job-template.yaml so capacity planning does not regress.
	jobCPURequest = "1"
	jobMemRequest = "2Gi"
	jobCPULimit   = "4"
	jobMemLimit   = "6Gi"
	jobRunAsUser  = int64(1000)
	jobRunAsGroup = int64(1000)
	jobFSGroup    = int64(1000)
)

// validateBaseRef enforces a safe base ref. PR authors can name
// their branch almost anything; only the subset of git refname
// characters that Boop legitimately needs is allowed through. The
// check is the gate between an attacker-controlled string and the
// Job spec; rejection is a 400 to the webhook.
func validateBaseRef(s string) error {
	if s == "" {
		return fmt.Errorf("base ref is empty")
	}
	if len(s) > 255 {
		return fmt.Errorf("base ref too long: %d bytes", len(s))
	}
	if !baseRefRegex.MatchString(s) {
		return fmt.Errorf("base ref contains unsafe characters: %q", s)
	}
	// A leading `-` is a flag-injection vector: the YAML
	// pipeline is gone, but the value still lands in the Job
	// name (and in any future code that passes it to a CLI).
	// Forbid it explicitly.
	if s[0] == '-' {
		return fmt.Errorf("base ref must not start with -: %q", s)
	}
	if s[0] == '/' || s[len(s)-1] == '/' {
		return fmt.Errorf("base ref must not start or end with /: %q", s)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("base ref must not contain ..: %q", s)
	}
	if strings.HasSuffix(s, ".lock") {
		return fmt.Errorf("base ref must not end with .lock: %q", s)
	}
	if strings.HasSuffix(s, ".") {
		return fmt.Errorf("base ref must not end with .: %q", s)
	}
	return nil
}

// validateInstallationIDString re-checks the formatted InstallationID
// before it lands in the Job spec. parseInstallationID already
// rejects non-integers, zero, and negative values when the header is
// parsed; this catches the case where the caller hands a pre-formatted
// string (e.g. "0" or "-1") and stops it from reaching K8s.
func validateInstallationIDString(s string) error {
	if s == "" {
		return fmt.Errorf("installation id is empty")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("installation id is not a valid integer: %q", s)
	}
	if n <= 0 {
		return fmt.Errorf("installation id must be positive: %d", n)
	}
	return nil
}

// validatePreviousHeadSHA accepts a hex SHA (7-40 chars) or the empty
// string (no prior review marker). Other shapes are rejected so the
// annotation is always either absent, empty, or a real SHA.
func validatePreviousHeadSHA(s string) error {
	if s == "" {
		return nil
	}
	if !hexSHAFullRegex.MatchString(s) {
		return fmt.Errorf("previous head SHA is not a valid hex SHA: %q", s)
	}
	return nil
}

// buildJob assembles a review Job as a typed batchv1.Job. This
// replaces the old strings.ReplaceAll + yaml.Unmarshal pattern,
// which let a PR author break out of a YAML scalar and inject
// arbitrary env vars into the runner pod (see C1 in the security
// review). With the Job built in Go the K8s API serialises every
// value with proper escaping; user input never re-enters the YAML
// pipeline.
func buildJob(v templateVars) (*batchv1.Job, error) {
	if err := validateBaseRef(v.BaseRef); err != nil {
		return nil, err
	}
	if err := validateInstallationIDString(v.InstallationID); err != nil {
		return nil, err
	}
	if err := validatePreviousHeadSHA(v.PreviousHeadSHA); err != nil {
		return nil, err
	}
	// Defense-in-depth: the K8s API serialises v.SHA safely today,
	// but the SHA also lands in the Job name and the runner's
	// assertSafeSha. Validating here closes the gap so a future
	// caller that reads the SHA from a less-typed source cannot
	// bypass the check.
	if err := validatePreviousHeadSHA(v.SHA); err != nil {
		return nil, fmt.Errorf("head SHA: %w", err)
	}
	number, err := strconv.Atoi(v.Number)
	if err != nil || number <= 0 {
		return nil, fmt.Errorf("pr number is not a positive integer: %q", v.Number)
	}

	ttl := int32(jobTTLSeconds)
	backoff := int32(jobBackoffLimit)
	deadline := int64(jobActiveDeadlineSecs)
	trueVal := true
	falseVal := false

	jobName := buildJobName(v.Owner, v.Repo, number, v.SHA)

	// Pod-level security context. runAsNonRoot + runAsUser 1000
	// are required for the distroless-style posture: a misbehaving
	// sub-process cannot gain uid 0 inside the pod. seccompProfile
	// RuntimeDefault is the upstream-recommended baseline; it
	// blocks a wide class of kernel-bound exploits without us
	// hand-rolling a profile.
	podSec := &corev1.PodSecurityContext{
		RunAsUser:    ptrInt64(jobRunAsUser),
		RunAsGroup:   ptrInt64(jobRunAsGroup),
		RunAsNonRoot: &trueVal,
		FSGroup:      ptrInt64(jobFSGroup),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}

	// Container-level security context. readOnlyRootFilesystem
	// is the single biggest hardening knob: every writable
	// location the runner needs becomes a mounted volume, so a
	// code-exec bug cannot persist artefacts. capabilities.drop
	// ["ALL"] + allowPrivilegeEscalation: false removes the
	// entire Linux capabilities surface. The seccomp profile
	// is repeated at the container level so policies that key
	// off one or the other both fire.
	contSec := &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   &trueVal,
		AllowPrivilegeEscalation: &falseVal,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}

	annotations := map[string]string{
		"boop/owner":             v.Owner,
		"boop/repo":              v.Repo,
		"boop/number":            v.Number,
		"boop/sha":               v.SHA,
		"boop/base-ref":          v.BaseRef,
		"boop/previous-head-sha": v.PreviousHeadSHA,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   jobNamespace,
			Labels:      map[string]string{"app": "boop", "pr-number": v.Number, "sha": v.SHA},
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "boop", "pr-number": v.Number},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: boopJobServiceAccount,
					SecurityContext:    podSec,
					Containers: []corev1.Container{
						{
							Name:  "review",
							Image: v.Image,
							// imagePullPolicy: IfNotPresent pairs with
							// digest-pinned images (see sync-image-digests
							// workflow). Skips the registry round-trip on
							// the happy path; still re-pulls on a tag
							// change because the kubelet compares the
							// resolved manifest digest, not the tag.
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{Name: "PR_OWNER", Value: v.Owner},
								{Name: "PR_REPO", Value: v.Repo},
								{Name: "PR_NUMBER", Value: v.Number},
								{Name: "PR_HEAD_SHA", Value: v.SHA},
								{Name: "PR_BASE_REF", Value: v.BaseRef},
								{Name: "PR_PREVIOUS_HEAD_SHA", Value: v.PreviousHeadSHA},
								{
									Name: "GITHUB_APP_ID",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: boopSecretsName},
											Key:                  "GITHUB_APP_ID",
										},
									},
								},
								{Name: "GITHUB_APP_INSTALLATION_ID", Value: v.InstallationID},
								// GITHUB_APP_PRIVATE_KEY and OPENROUTER_API_KEY
								// are NOT exposed as env. They are mounted as
								// files (defaultMode 0400) and the runner
								// reads them via fs.readFile. A
								// prompt-injected LLM reaching them via
								// /proc/self/environ or the opencode env
								// tool would otherwise be a trivial secret
								// exfil path.
								{Name: "BOOP_STATUS_COMMENT_ID", Value: v.StatusCommentID},
								{Name: "BOOP_REACTION_COMMENT_ID", Value: v.ReactionCommentID},
								{Name: "BOOP_REVIEW_NUMBER", Value: v.ReviewNumber},
								{Name: "BOOP_BOT_LOGIN", Value: v.BotLogin},
								{Name: "BOOP_SKIP_SKILL", Value: "0"},
								// Tell the runner where on disk to find
								// each mounted secret. The runner reads
								// the file at process start and stops
								// referencing the path.
								{Name: "BOOP_GITHUB_APP_PRIVATE_KEY_PATH", Value: privateKeyMountPath},
								{Name: "BOOP_OPENROUTER_API_KEY_PATH", Value: openrouterKeyMountPath},
							},
							SecurityContext: contSec,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(jobCPURequest),
									corev1.ResourceMemory: resource.MustParse(jobMemRequest),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(jobCPULimit),
									corev1.ResourceMemory: resource.MustParse(jobMemLimit),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      runnerConfigVolume,
									ReadOnly:  true,
									MountPath: runnerConfigMountPath,
								},
								{
									Name:     privateKeyVolume,
									ReadOnly: true,
									// SubPath pins the mount to a single
									// key from the Secret. Without it the
									// whole Secret would appear as a
									// directory of files; SubPath scopes
									// the exposure to the one key.
									SubPath:   "GITHUB_APP_PRIVATE_KEY",
									MountPath: privateKeyMountPath,
								},
								{
									Name:      openrouterKeyVolume,
									ReadOnly:  true,
									SubPath:   "OPENROUTER_API_KEY",
									MountPath: openrouterKeyMountPath,
								},
								{Name: workVolume, MountPath: workMountPath},
								{Name: tmpVolume, MountPath: tmpMountPath},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: runnerConfigVolume,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: boopRunnerConfigConfigMap},
									Items: []corev1.KeyToPath{
										{Key: "opencode.json", Path: "opencode.json"},
										{Key: "skill-boop", Path: "skills/boop/SKILL.md"},
										{Key: "skill-boop-agent-code-quality", Path: "skills/boop/agents/review-code-quality.md"},
										{Key: "skill-boop-agent-design-pattern", Path: "skills/boop/agents/review-design-pattern.md"},
										{Key: "skill-boop-agent-error-handling", Path: "skills/boop/agents/review-error-handling.md"},
										{Key: "skill-boop-agent-readability", Path: "skills/boop/agents/review-readability.md"},
										{Key: "skill-boop-agent-solid-principles", Path: "skills/boop/agents/review-solid-principles.md"},
										{Key: "skill-boop-agent-test-quality", Path: "skills/boop/agents/review-test-quality.md"},
										{Key: "skill-boop-agent-deep", Path: "skills/boop/agents/review-deep.md"},
									},
								},
							},
						},
						{
							Name: privateKeyVolume,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  boopSecretsName,
									DefaultMode: ptrInt32(secretFileMode),
									Items: []corev1.KeyToPath{
										{Key: "GITHUB_APP_PRIVATE_KEY", Path: "GITHUB_APP_PRIVATE_KEY"},
									},
								},
							},
						},
						{
							Name: openrouterKeyVolume,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  boopSecretsName,
									DefaultMode: ptrInt32(secretFileMode),
									Items: []corev1.KeyToPath{
										{Key: "OPENROUTER_API_KEY", Path: "OPENROUTER_API_KEY"},
									},
								},
							},
						},
						{
							Name: workVolume,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: tmpVolume,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	return job, nil
}

func ptrInt32(v int32) *int32 { return &v }
func ptrInt64(v int64) *int64 { return &v }
