package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FetchPodLogs returns the most-recent pod's logs for the
// given Job name in the receiver's namespace. Used by the
// dashboard's run-detail view to surface the runner's
// stdout/stderr to the operator.
//
// The receiver does not persist pod logs itself — K8s
// garbage-collects the Job (and its pod) 1 hour after the
// Job's CompletionTime, so for older runs the call returns
// ("", nil) and the dashboard renders "(Job TTL'd; logs
// unavailable)". The dashboard treats a not-found pod as
// "logs unavailable" rather than an error.
//
// The caller is the dashboard package (via the Actions
// callback); the receiver never invokes this itself. The
// method is exported because it's wired into the dashboard
// at startup in main.go.
func (h *Handler) FetchPodLogs(ctx context.Context, jobName string) (string, error) {
	if h.kube == nil {
		return "", fmt.Errorf("webhook: FetchPodLogs: kube client not configured")
	}
	if h.cfg.TargetNamespace == "" {
		return "", fmt.Errorf("webhook: FetchPodLogs: target namespace unset")
	}

	// Find the pod for this Job. Pick the most-recent one
	// when a Job has retried (same selector the reconciler
	// uses for failure_class).
	pods, err := h.kube.CoreV1().Pods(h.cfg.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("webhook: FetchPodLogs: list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		// Job TTL'd out — not an error, the dashboard
		// handles this as "logs unavailable".
		return "", nil
	}
	var latest *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if latest == nil || podStartAfter(p, latest) {
			latest = p
		}
	}
	if latest == nil {
		return "", nil
	}

	// Bound the read so a runaway log line (the runner
	// uses script(1) PTY, so terminal escapes can blow
	// up line counts) cannot hang the dashboard request.
	logCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req := h.kube.CoreV1().Pods(h.cfg.TargetNamespace).GetLogs(latest.Name, &corev1.PodLogOptions{
		// Previous=true would surface the previous
		// container's logs after a CrashLoopBackOff;
		// the runner's first start is usually the
		// interesting one, but the most-recent is
		// what an operator debugging today wants.
		Previous: false,
	})
	stream, err := req.Stream(logCtx)
	if err != nil {
		return "", fmt.Errorf("webhook: FetchPodLogs: stream: %w", err)
	}
	defer stream.Close()

	// Cap the bytes we keep. A typical runner log is 5-50
	// KiB; a misbehaving lens can emit megabytes. 2 MiB
	// keeps the dashboard page under control and is far
	// above any healthy run's output.
	var buf bytes.Buffer
	_, err = io.Copy(&buf, io.LimitReader(stream, 2<<20))
	if err != nil {
		// context.DeadlineExceeded is fine — return
		// what we got rather than failing the request.
		return buf.String(), nil
	}
	return buf.String(), nil
}
