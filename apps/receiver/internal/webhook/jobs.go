package webhook

// K8s controller layer (RF-002 split).
//
// The methods on `*Handler` that talk to the K8s API
// (createJob, deleteJob, jobStatus), read the boop-config
// ConfigMap (currentJobImage, resolveJobImageForSubmit,
// bumpConfigMapFallbacks, resetConfigMapFallbacks), and
// resolve the per-PR SDK flag (resolveSDKEnabled) used
// to live in handler.go alongside the HTTP / dispatch
// code. After this split, the K8s controller surface is
// here so handler.go is a slimmer HTTP + dedup file.
//
// The methods stay on `*Handler` because they need
// `h.kube`, `h.cfg`, `h.logger`, and `h.store`. They
// could move to a new `jobs/` package, but the methods
// are few and the cost of cross-package wiring
// (constructor exposure, interface boundary) is higher
// than the LOC savings. A future second-cluster rollout
// can revisit the package split; today the file split
// alone cuts handler.go to its HTTP surface.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const boopConfigMapName = "boop-config"

// jobImageKey is the data key under which JOB_IMAGE is stored in
// the boop-config ConfigMap. Must match apps/k8s/base/config.yaml.
const jobImageKey = "JOB_IMAGE"

// sdkEnabledLabel is the GitHub label that opts a PR into the
// OpenRouter SDK path for its next review. The cluster-wide
// default (Config.OpenRouterSDKDefault, sourced from
// BOOP_USE_OPENROUTER_SDK on the receiver) still applies; the
// label is a per-PR override. QUB-98 made the SDK the runner's
// only invocation path; the label is preserved for the QUB-N
// rollout, where clusters that have not yet flipped the default
// to "1" still route through the SDK in the runner.
const sdkEnabledLabel = "boop:openrouter-sdk"

// jobStatus returns one of: "missing", "active", "failed", "succeeded".
// Used to decide whether to create, skip (duplicate), or replace
// (failed) a Job for a given head SHA.
func (h *Handler) jobStatus(ctx context.Context, name string) (string, error) {
	job, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return "missing", nil
		}
		return "", err
	}
	if job.Status.Failed > 0 {
		return "failed", nil
	}
	if job.Status.Succeeded > 0 {
		return "succeeded", nil
	}
	return "active", nil
}

func (h *Handler) deleteJob(ctx context.Context, name string) error {
	propagation := metav1.DeletePropagationBackground
	return h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
}

func (h *Handler) createJob(ctx context.Context, job *batchv1.Job) error {
	_, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

// currentJobImage reads JOB_IMAGE from the boop-config ConfigMap
// in the receiver's namespace and returns the freshest value ArgoCD
// has synced. Falls back to the env-var snapshot (h.cfg.JobImage)
// when the read fails so a transient K8s API hiccup doesn't block
// a review. Not cached: the K8s API round-trip is cheap relative to
// the rest of the webhook handler (token mint + GitHub API calls),
// and skipping the cache keeps the recovery time at "next webhook"
// after ArgoCD syncs a new digest.
//
// The receiver's Role grants configmaps:get/list/watch in
// TARGET_NAMESPACE — see apps/k8s/base/role.yaml. The ConfigMap is
// namespace-scoped, so the receiver can only read it from its own
// namespace, which is the right blast-radius.
func (h *Handler) currentJobImage(ctx context.Context) (string, error) {
	cm, err := h.kube.CoreV1().ConfigMaps(h.cfg.TargetNamespace).Get(ctx, boopConfigMapName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s/%s: %w", h.cfg.TargetNamespace, boopConfigMapName, err)
	}
	v, ok := cm.Data[jobImageKey]
	if !ok || v == "" {
		return "", fmt.Errorf("%s/%s missing %q key", h.cfg.TargetNamespace, boopConfigMapName, jobImageKey)
	}
	return v, nil
}

// resolveJobImageForSubmit wraps currentJobImage with the
// fallback-to-env-snapshot policy and the consecutive-failure
// counter. Always returns a non-empty image suitable for the Job
// template: a successful ConfigMap read returns the live digest;
// a failed read returns the env-var snapshot and increments the
// counter (escalating to Error-level logging after
// consecutiveFallbackAlertAt). On success, the counter resets to
// 0 — a single successful read is enough to declare "the API is
// fine, the previous failures were transient."
//
// Returned source is "configmap" or "fallback" so tests can
// assert the path taken without inspecting logs.
func (h *Handler) resolveJobImageForSubmit(ctx context.Context) (image, source string) {
	cm, err := h.currentJobImage(ctx)
	if err == nil {
		h.resetConfigMapFallbacks()
		return cm, "configmap"
	}
	count := h.bumpConfigMapFallbacks()
	level := slog.LevelWarn
	if count >= consecutiveFallbackAlertAt {
		level = slog.LevelError
	}
	h.logger.Log(ctx, level, "read boop-config for JOB_IMAGE, using startup value",
		"err", err,
		"consecutive_fallbacks", count,
		"alert_threshold", consecutiveFallbackAlertAt,
	)
	return h.cfg.JobImage, "fallback"
}

// bumpConfigMapFallbacks increments the consecutive-fallback
// counter under a lock and returns the new value. The lock is a
// separate mutex (not the dedup mutex) so the read-then-increment
// pattern stays a single critical section.
func (h *Handler) bumpConfigMapFallbacks() int {
	h.consecutiveConfigMapFallbacksLock.Lock()
	defer h.consecutiveConfigMapFallbacksLock.Unlock()
	h.consecutiveConfigMapFallbacks++
	return h.consecutiveConfigMapFallbacks
}

// resetConfigMapFallbacks zeroes the counter on a successful
// ConfigMap read. Lock-protected for the same reason as bump.
func (h *Handler) resetConfigMapFallbacks() {
	h.consecutiveConfigMapFallbacksLock.Lock()
	defer h.consecutiveConfigMapFallbacksLock.Unlock()
	h.consecutiveConfigMapFallbacks = 0
}

func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// resolveSDKEnabled picks the BOOP_USE_OPENROUTER_SDK value for
// the next review Job on a PR. The cluster default
// (h.cfg.OpenRouterSDKDefault) sets the floor; the per-PR label
// is an opt-in. The decision is logged so the operator can see
// why a given Job landed on either value.
func (h *Handler) resolveSDKEnabled(labels []string) string {
	if hasLabel(labels, sdkEnabledLabel) {
		return "1"
	}
	if h.cfg.OpenRouterSDKDefault == "1" {
		return "1"
	}
	return "0"
}
