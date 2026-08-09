package webhook

// K8s Job reconciler (QUB-108).
//
// The receiver creates a K8s Job at submitJob time, but the
// Job runs in a pod that the receiver does not own. Without
// a reconciler, the dashboard's failure_class column is
// forever empty (the runner POSTs the application-level
// error string in UpdateRunStatus, but never the K8s
// container exit reason). The exception dock's first
// deliverable — "OOMKilled runs light up on day one" —
// requires the exit reason to land in the row.
//
// The reconciler polls Jobs in the receiver's namespace on a
// fixed cadence. For each Job whose status is terminal
// (Succeeded or Failed > 0), it reads the pod's last
// container state, extracts the exit reason
// (OOMKilled/Error/Completed/etc.), and writes it into the
// matching runs row via SetRunFailureClass.

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// failureClassFromContainerState maps a pod's last container
// state to a dashboard failure_class string. The mapping
// follows the Phase 1 spec's exception-dock filter values:
//
//   - OOMKilled           → "oom_killed"
//   - Error               → "container_error"
//   - Completed (exit 0)  → "" (success, no class)
//   - CrashLoopBackOff    → "crash_loop"
//   - ImagePullBackOff    → "image_pull"
//   - anything else       → "container_<reason>"
//
// The values are the dashboard's filter chips, not K8s
// internals — keeping the K8s surface contained inside this
// function lets the dashboard evolve its taxonomy without
// touching the reconciler. Empty string means "Job exited
// cleanly"; SetRunFailureClass stores the empty string and
// the dashboard renders the row without a class pill.
func failureClassFromContainerState(cs corev1.ContainerState) string {
	if t := cs.Terminated; t != nil {
		return mapExitReason(t.Reason, t.ExitCode)
	}
	if w := cs.Waiting; w != nil {
		// Waiting means the pod never reached a terminal
		// state, but a stuck ImagePullBackOff /
		// CrashLoopBackOff still tells the dashboard
		// something — the row gets a "stuck" class so the
		// exception dock can highlight it. Without this,
		// an infinite CrashLoopBackOff silently shows up
		// as "running" forever.
		return mapWaitingReason(w.Reason)
	}
	return ""
}

func mapExitReason(reason string, exitCode int32) string {
	switch reason {
	case "":
		// "Completed" is the empty-reason case. Exit 0 is
		// a clean run; non-zero with no reason is the
		// generic Error case.
		if exitCode == 0 {
			return ""
		}
		return "container_error"
	case "Completed":
		return ""
	case "OOMKilled":
		return "oom_killed"
	case "Error":
		return "container_error"
	default:
		// Unknown reason — preserve the K8s value so an
		// operator can grep for it. Prefixed so it can't
		// collide with the dashboard's controlled
		// vocabulary.
		return "container_" + reason
	}
}

func mapWaitingReason(reason string) string {
	switch reason {
	case "CrashLoopBackOff":
		return "crash_loop"
	case "ImagePullBackOff", "ErrImagePull":
		return "image_pull"
	case "CreateContainerConfigError":
		return "config_error"
	default:
		if reason == "" {
			return ""
		}
		return "stuck_" + reason
	}
}

// StartJobReconciler kicks off a background loop that polls
// K8s Jobs in the receiver's namespace and backfills the
// failure_class column on terminal Jobs. It is best-effort:
// every tick is independent and any error is logged and
// swallowed. The returned cancel func stops the goroutine
// (safe to call multiple times).
//
// interval is the polling cadence. 0 → 30s default. The
// floor is 5s — a tight poll against the K8s API is
// wasteful and the dashboard's exception-dock view does
// not need sub-5s freshness.
//
// Why a poll instead of a watch/informer: the receiver is
// one replica and watches against its own in-cluster API
// server; an informer is the textbook answer but the
// restart-recovery complexity is not worth the freshness
// gain. A 30s poll is well below the operator's
// "I clicked the dashboard and didn't see the OOM yet"
// patience threshold.
// orphanGraceSeconds is the floor at which a "running" row with
// no heartbeat is considered orphaned by the reconciler. The
// dashboard's admin endpoint uses 5 minutes for its
// mark-orphaned bulk action; the reconciler needs a longer
// grace to avoid racing with a healthy in-flight review (a
// review can take 5-15 minutes to reach the first heartbeat on
// a slow OpenRouter call). 1 hour matches the K8s Job's
// TTLSecondsAfterFinished (jobbuilder.go:jobTTLSeconds = 3600),
// so any run whose Job has been TTL'd out is a candidate.
const orphanGraceSeconds = 3600

func (h *Handler) StartJobReconciler(ctx context.Context, interval time.Duration) func() {
	if h.store == nil || h.kube == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	h.logger.Info("job reconciler starting", "interval", interval, "orphan_grace_seconds", orphanGraceSeconds)
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			tickCtx, tickCancel := context.WithTimeout(pollerCtx, 30*time.Second)
			// QUB-135: a second pass that catches runs whose
			// K8s Job has TTL'd out. reconcileJobsOnce only
			// sees Jobs still in the namespace; once the Job
			// is gone (TTL expired, GC'd, or never created
			// because the receiver crashed), the run is
			// orphaned. MarkOrphanedRuns queries the store
			// directly and marks any "running" row with no
			// heartbeat and older than the grace as failed
			// with a synthetic "orphaned" reason. The grace
			// (1h) matches jobTTLSeconds so a healthy review
			// that just hasn't heartbeated yet is never
			// caught.
			n, err := h.store.MarkOrphanedRuns(tickCtx, orphanGraceSeconds*time.Second)
			if err != nil {
				h.logger.Warn("reconcile mark orphaned", "err", err)
			} else if n > 0 {
				h.logger.Info("reconciled orphaned runs", "count", n)
			}
			h.reconcileJobsOnce(tickCtx)
			tickCancel()
			t.Reset(interval)
		}
	}()
	return cancel
}

// reconcileJobsOnce runs one pass of the reconciler. It
// lists Jobs in the receiver's namespace, filters to
// terminal ones (succeeded or failed > 0), reads the
// matching pod's last container state, and backfills
// failure_class. Exposed (lowercase, package-internal) so
// the integration tests can drive a single pass without
// spawning a goroutine.
func (h *Handler) reconcileJobsOnce(ctx context.Context) {
	jobs, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		h.logger.Warn("reconcile list jobs", "err", err)
		return
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !jobIsTerminal(job) {
			continue
		}
		name := job.Name
		fc, err := h.classifyJobExit(ctx, name)
		if err != nil {
			h.logger.Warn("reconcile classify", "job", name, "err", err)
			continue
		}
		if err := h.store.SetRunFailureClass(ctx, name, fc); err != nil {
			// sql.ErrNoRows = the run was pruned (retention
			// race). Not a reconciler error, just nothing
			// to do. We log at Debug so the noise floor
			// stays low.
			h.logger.Debug("reconcile set failure class", "job", name, "err", err)
		}
		// QUB-114: also reconcile the run's terminal status.
		// Previously this loop only wrote failure_class,
		// which left status="running" forever on any run
		// whose runner never called /api/runs/{id}/status
		// (a container_error / OOMKilled job where the
		// process died before reaching the
		// post-summary status post). The dashboard's
		// live view showed these as in-progress until
		// the 365-day retention tick pruned them.
		// UpdateRunStatusIfRunning is a no-op when the
		// runner already finalised the row, so a healthy
		// run that finished via the normal
		// stage → done transition is never overwritten.
		status, errMsg, endedAt := deriveTerminalState(job)
		if status == "" {
			continue
		}
		written, err := h.store.UpdateRunStatusIfRunning(ctx, name, status, endedAt, nil, errMsg)
		if err != nil {
			h.logger.Debug("reconcile set status", "job", name, "err", err)
			continue
		}
		if written {
			h.logger.Info("reconciled orphaned run", "job", name, "status", status)
		}
	}
}

// deriveTerminalState maps a terminal K8s Job to the
// (status, error, ended_at) triple to write into the runs
// row. Returns ("", nil, nil) when the Job is not actually
// terminal (e.g. Failed=0/Succeeded=0 with only the Active
// counter ticking down — K8s marks the conditions a beat
// late, so callers should check jobIsTerminal first).
//
// The error message comes from the Job's Failed condition
// when present ("BackoffLimitExceeded" / "DeadlineExceeded"
// / a custom message), and from a synthetic
// "reconciled: <N> failed pods" line otherwise. The
// empty-status guard lets callers skip the write for a Job
// that the conditions haven't settled on yet.
func deriveTerminalState(job *batchv1.Job) (store.RunStatus, string, *time.Time) {
	var (
		status  store.RunStatus
		errMsg  string
		endedAt *time.Time
	)
	if job.Status.CompletionTime != nil {
		t := job.Status.CompletionTime.Time
		endedAt = &t
	}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			status = store.StatusSucceeded
		case batchv1.JobFailed:
			status = store.StatusFailed
			if c.Reason != "" {
				errMsg = c.Reason
				if c.Message != "" {
					errMsg = c.Reason + ": " + c.Message
				}
			} else if c.Message != "" {
				errMsg = c.Message
			} else if job.Status.Failed > 0 {
				errMsg = fmt.Sprintf("reconciled: %d failed pods", job.Status.Failed)
			}
		}
	}
	if status == "" && job.Status.Failed > 0 {
		status = store.StatusFailed
		if errMsg == "" {
			errMsg = fmt.Sprintf("reconciled: %d failed pods", job.Status.Failed)
		}
	}
	if status == "" && job.Status.Succeeded > 0 {
		status = store.StatusSucceeded
	}
	return status, errMsg, endedAt
}

// jobIsTerminal reports whether the Job is in a state
// worth reconciling. Both "Succeeded > 0" and "Failed > 0"
// are terminal; "Active > 0" is not.
func jobIsTerminal(j *batchv1.Job) bool {
	return j.Status.Succeeded > 0 || j.Status.Failed > 0
}

// classifyJobExit reads the pod for the given Job and
// returns the failure_class to write. Returns an empty
// string if the Job's pod never reached a terminal state
// (e.g. the pod is still pending) — SetRunFailureClass
// will store it, the dashboard will show no class, and a
// later tick will re-evaluate when the pod settles.
func (h *Handler) classifyJobExit(ctx context.Context, jobName string) (string, error) {
	pods, err := h.kube.CoreV1().Pods(h.cfg.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", err
	}
	// No pods yet — the Job might be queued. Return
	// empty so the reconciler leaves the row alone for
	// now and tries again next tick.
	if len(pods.Items) == 0 {
		return "", nil
	}
	// Pick the most recent pod by start time. A
	// retry-on-failure Job creates multiple pods over
	// its lifetime; we want the LAST one because that's
	// the one whose exit reason should classify the
	// run.
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
	// Walk the container states in reverse so the
	// LAST state (e.g. Terminated after a Waiting
	// ImagePullBackOff) wins. State.Terminated is
	// preferred over State.Waiting because a
	// terminated container's exit reason is the
	// authoritative classification.
	for i := len(latest.Status.ContainerStatuses) - 1; i >= 0; i-- {
		c := latest.Status.ContainerStatuses[i]
		if cls := failureClassFromContainerState(c.LastTerminationState); cls != "" {
			return cls, nil
		}
		if cls := failureClassFromContainerState(c.State); cls != "" {
			return cls, nil
		}
	}
	return "", nil
}

// podStartAfter reports whether p started strictly after q.
// Used to pick the most recent pod when a Job has retried.
func podStartAfter(p, q *corev1.Pod) bool {
	if p.Status.StartTime == nil {
		return false
	}
	if q.Status.StartTime == nil {
		return true
	}
	return p.Status.StartTime.After(q.Status.StartTime.Time)
}
