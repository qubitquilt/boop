package webhook

// Re-run API (QUB-110).
//
// The dashboard's "Re-run" button is the only legitimate
// way to mint a new Job for the same head SHA after the
// original has terminated. The HTTP endpoints are:
//
//   GET  /api/runs/{id}/rerun-preview
//        Returns the diff the operator is about to
//        confirm — model, head SHA, prior status,
//        prior run duration, the proposed new Job
//        name.
//
//   POST /api/runs/{id}/rerun
//        Body: {confirm: true, reason}
//        Mints a new Job that bypasses claimJobSlot
//        (operator-initiated, allowed to coexist with
//        other re-runs), points parent_run_id at the
//        prior run, and writes superseded_by_id on the
//        prior row.
//
// Both endpoints require the BOOP_DASHBOARD_TOKEN
// middleware (Phase 4 wires the gate; for Phase 3 the
// check is a stub that returns 404 — the API surface
// exists but is not yet callable from the dashboard).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// rerunPreviewResponse is the body of GET
// /api/runs/{id}/rerun-preview. The dashboard renders
// this as a diff table with a confirm-tick + reason
// field before POSTing to /rerun.
type rerunPreviewResponse struct {
	Prior rerunPreviewRun `json:"prior"`
	New   rerunPreviewRun `json:"new"`
}

// rerunPreviewRun is the per-run slice of the preview.
type rerunPreviewRun struct {
	RunID     string `json:"run_id"`
	JobName   string `json:"job_name"`
	Status    string `json:"status"`
	Model     string `json:"model"`
	HeadSHA   string `json:"head_sha"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Duration  int64  `json:"duration_ms,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// RerunPreview handles GET /api/runs/{id}/rerun-preview.
// Returns 404 for an unknown run, 409 for a run whose
// status is not succeeded/failed. The dashboard should
// hide the re-run button in non-terminal states; the 409
// is a defensive belt-and-braces so a stale tab still
// surfaces a clear error.
func (h *Handler) RerunPreview(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	prior, err := h.store.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("rerun preview get", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if prior.Status != store.StatusSucceeded && prior.Status != store.StatusFailed {
		http.Error(w, "can only re-run a terminal run", http.StatusConflict)
		return
	}
	telem, _ := h.store.GetTelemetry(r.Context(), id)
	preview := rerunPreviewResponse{
		Prior: rerunPreviewRun{
			RunID:   prior.ID,
			JobName: prior.ID,
			Status:  string(prior.Status),
			HeadSHA: prior.CommitSHA,
			Reason:  prior.Reason,
		},
	}
	if !prior.StartedAt.IsZero() {
		preview.Prior.StartedAt = prior.StartedAt.Format(time.RFC3339)
	}
	if prior.EndedAt != nil {
		preview.Prior.EndedAt = prior.EndedAt.Format(time.RFC3339)
	}
	if prior.DurationMS != nil {
		preview.Prior.Duration = *prior.DurationMS
	}
	if telem.Model != "" {
		preview.Prior.Model = telem.Model
	}
	sha7 := store.ShortSHA(prior.CommitSHA)
	count, err := h.store.CountRerunJobsForSHA(r.Context(), prior.Owner, prior.Repo, prior.PRNumber, sha7)
	if err != nil {
		h.logger.Warn("rerun preview count", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	newName := buildRerunJobName(prior.Owner, prior.Repo, prior.PRNumber, sha7, count+1)
	preview.New = rerunPreviewRun{
		RunID:   newName,
		JobName: newName,
		Status:  string(store.StatusPending),
		Model:   telem.Model,
		HeadSHA: prior.CommitSHA,
	}
	writeJSON(w, http.StatusOK, preview)
}

// rerunRequest is the body of POST /api/runs/{id}/rerun.
// `confirm: true` is required (defense against a CSRF
// or a stale dashboard tab). `reason` is a free-form
// string the operator fills in; it's logged in the audit
// trail and surfaced on the new run's row.
type rerunRequest struct {
	Confirm bool   `json:"confirm"`
	Reason  string `json:"reason"`
}

// Rerun handles POST /api/runs/{id}/rerun. Mints a new
// run row with parent_run_id set and creates the K8s
// Job in one transaction. The Job name follows the
// {original}-r{N} convention; the runner is told the
// prior run's id via BOOP_PARENT_RUN_ID so it can reuse
// the prior review's findings (Phase 4 follow-up).
//
// Every successful Rerun appends an audit row tagged
// with the runner-derived actor. The dashboard's
// serveRerun (form path) and this handler (API path)
// write the same action type so a compliance review can
// see "who re-ran this?" across both entry points.
// Without the API-side write, a runner-initiated rerun
// (e.g. an automated tool calling the API) is invisible
// in the audit_events table — a gap a future reviewer
// would flag as "no entry point for non-operator
// actions". The actor is a SHA-256 prefix of the
// RunnerToken, the same shape the dashboard uses for
// BOOP_DASHBOARD_TOKEN, so the row is traceable to a
// token (not to a user) but stable across calls.
// rerunResponse is the body of POST /api/runs/{id}/rerun.
// The shape is stable: every field is a documented part of
// the API contract, and clients can decode into this struct
// without `map[string]any` round-trips. Adding a new field
// is a one-call-site change with the JSON tag as the
// single source of truth. The earlier shape used a
// free-form map; future clients could not rely on the
// field set, and a "permanent note" field that was
// always empty lived in the wire shape as a permanent
// drift. (EH-009.)
type rerunResponse struct {
	NewRunID    string `json:"new_run_id"`
	PriorRunID  string `json:"prior_run_id"`
	ParentRunID string `json:"parent_run_id"`
}

func (h *Handler) Rerun(w http.ResponseWriter, r *http.Request) {
	// QUB-127: auth gate. Every other dashboard POST
	// handler calls checkRunnerToken(r) before any work.
	// The rerun handler skipped it, which meant an
	// unauthenticated POST against the QUB-115 public
	// surface created a real K8s Job with the runner
	// image and the secrets that come with it. The
	// check is constant-time compare on X-BOOP-Runner-Token
	// against Config.RunnerToken — same pattern as
	// RecordTelemetry / RecordStatus / RecordStage.
	if !h.checkRunnerToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	var body rerunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !body.Confirm {
		http.Error(w, "confirm: true required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	prior, err := h.store.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("rerun get", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if prior.Status != store.StatusSucceeded && prior.Status != store.StatusFailed {
		http.Error(w, "can only re-run a terminal run", http.StatusConflict)
		return
	}
	newID, err := h.CreateRerunJob(r.Context(), prior, body.Reason)
	if err != nil {
		h.logger.Warn("rerun create", "run", id, "err", err)
		http.Error(w, "rerun create", http.StatusInternalServerError)
		return
	}
	if _, err := h.store.RecordAuditEvent(r.Context(), store.AuditEvent{
		Action:   "rerun.create",
		Actor:    h.runnerActor(),
		TargetID: newID,
		Details: store.MarshalDetails(map[string]any{
			"prior_run_id": id,
			"reason":       body.Reason,
			"source":       "api",
		}),
	}); err != nil {
		// Non-fatal: the dashboard's audit view will
		// surface a gap, which is preferable to a
		// 5xx that loses the operator's requeue.
		h.logger.Warn("rerun audit", "run", newID, "err", err)
	}
	h.logger.Info("rerun created",
		"prior", prior.ID,
		"new", newID,
		"reason", body.Reason,
	)
	writeJSON(w, http.StatusAccepted, rerunResponse{
		NewRunID:    newID,
		PriorRunID:  prior.ID,
		ParentRunID: prior.ID,
	})
}

// runnerActor derives the audit-log actor for an
// API-initiated runner action. Mirrors the dashboard's
// actor() via the shared store.ActorFromToken (SHA-256
// prefix of the token), but prefixed "runner:" so a
// compliance view can split the two entry points in one
// query. Empty RunnerToken yields "runner:unconfigured"
// — the auth gate rejects such callers before this
// point, so the value never lands in audit_events.
func (h *Handler) runnerActor() string {
	if h.cfg.RunnerToken == "" {
		return "runner:unconfigured"
	}
	return store.ActorFromToken("runner:", h.cfg.RunnerToken)
}

// CreateRerunJob is the cross-package helper the
// dashboard calls from its form-based /dashboard/runs/
// {id}/rerun handler. Persists the new run row with
// parent_run_id set, backfills superseded_by_id on the
// prior, and creates the K8s Job in the configured
// namespace. Returns the new run id.
//
// The K8s Job's name is the {original}-r{N} convention;
// the new row's id matches the Job name so the
// runner's BOOP_JOB_NAME environment variable lines up
// with the store row without a translation. BOOP_PARENT_RUN_ID
// is set so a future runner change can read the prior
// row's findings (Phase 4 follow-up — today the runner
// ignores the env var).
//
// The new run row is persisted BEFORE the K8s Job is
// created, matching the QUB-101 order in submitJob: the
// runner's first POST can find the parent row by the
// time the Job starts. The persistence path uses
// CreateRerun, which writes the new row and the prior's
// superseded_by_id pointer in one transaction so a crash
// between the two cannot leave a half-linked chain
// (EH-005). If the prior was retention-pruned between
// the operator's click and the write, CreateRerun
// returns sql.ErrNoRows and the new row still lands —
// the WalkUp view still works via parent_run_id, only
// the WalkDown pill on the prior goes missing. If
// UpsertRun-class failure happens, no K8s side effect
// is attempted. If createJob fails, the new row stays
// in pending state; the receiver's retention loop will
// eventually prune it. A re-run is operator-initiated
// so the "stuck pending" surface is small (one row, one
// log line).
func (h *Handler) CreateRerunJob(ctx context.Context, prior store.Run, reason string) (string, error) {
	if h.store == nil {
		return "", fmt.Errorf("webhook: rerun: data layer disabled")
	}
	if h.kube == nil {
		return "", fmt.Errorf("webhook: rerun: kube client not configured")
	}
	// A re-run only makes sense for terminal prior
	// rows. Re-running a still-running prior would
	// either duplicate the live Job (same
	// owner/repo/pr/sha, no -r{N} yet) or race the
	// inflight pod's first POST. Both shapes are
	// worse than a 409, so we reject at the boundary.
	if prior.Status != store.StatusSucceeded && prior.Status != store.StatusFailed {
		return "", fmt.Errorf("webhook: rerun: prior status %q is not terminal", prior.Status)
	}
	sha7 := store.ShortSHA(prior.CommitSHA)
	// EH-008: the count + insert has a TOCTOU race
	// when two re-runs of the same prior race. We
	// retry with count+1 when the store's UNIQUE
	// constraint fires; the bounded loop caps the
	// "stuck" surface if the race is somehow hotter
	// than the design assumes.
	const maxRerunAttempts = 8
	var newName string
	for attempt := 0; attempt < maxRerunAttempts; attempt++ {
		count, err := h.store.CountRerunJobsForSHA(ctx, prior.Owner, prior.Repo, prior.PRNumber, sha7)
		if err != nil {
			return "", fmt.Errorf("count reruns: %w", err)
		}
		newName = buildRerunJobName(prior.Owner, prior.Repo, prior.PRNumber, sha7, count+1)
		_, err = h.store.CreateRerun(ctx, store.Run{
			ID:             newName,
			Owner:          prior.Owner,
			Repo:           prior.Repo,
			PRNumber:       prior.PRNumber,
			CommitSHA:      prior.CommitSHA,
			BaseRef:        prior.BaseRef,
			ReviewNumber:   prior.ReviewNumber + 1,
			Reason:         "rerun: " + reason,
			InstallationID: prior.InstallationID,
			Status:         store.StatusPending,
			StartedAt:      time.Now().UTC(),
			ParentRunID:    prior.ID,
		}, prior.ID)
		if err == nil {
			break
		}
		if errors.Is(err, store.ErrDuplicateRerunName) {
			h.logger.Info("rerun name collision, retrying", "prior", prior.ID, "attempt", attempt+1, "name", newName)
			continue
		}
		return "", fmt.Errorf("create rerun: %w", err)
	}
	// Resolve the image fresh from the boop-config
	// ConfigMap so the rerun picks up the same
	// digest-sync updates the original submit path
	// already does. A failure here falls back to the
	// env-var snapshot (same as submitJob).
	image, _ := h.resolveJobImageForSubmit(ctx)
	job, err := buildJob(templateVars{
		Owner:             prior.Owner,
		Repo:              prior.Repo,
		Number:            fmt.Sprintf("%d", prior.PRNumber),
		SHA:               prior.CommitSHA,
		SHA7:              store.ShortSHA(prior.CommitSHA),
		BaseRef:           prior.BaseRef,
		PreviousHeadSHA:   "", // reruns do not chain; the prior's previous head SHA is irrelevant
		Image:             image,
		StatusCommentID:   "", // reruns do not get the original's status comment
		ReactionCommentID: "",
		ReviewNumber:      fmt.Sprintf("%d", prior.ReviewNumber+1),
		InstallationID:    fmt.Sprintf("%d", prior.InstallationID),
		BotLogin:          h.cfg.BotLogin,
		JobName:           newName,
		// QUB-94: re-runs reuse the cluster-wide SDK
		// default. Per-PR label overrides (boop:openrouter-sdk)
		// are not honored on a re-run because the
		// webhook handler is no longer in the loop; a
		// future "rerun with override" endpoint can
		// add a per-rerun flag.
		OpenRouterSDKEnabled: h.cfg.OpenRouterSDKDefault,
		// QUB-106: forward the receiver's OPENROUTER_MODEL
		// to the rerun Job. The runner reads it as
		// ctx.openrouterModel and calls the OpenRouter SDK
		// with it. Without this, the jobbuilder sets
		// OPENROUTER_MODEL="" and the runner's expert
		// dispatch throws `callOpenRouter: 'model' is
		// required` (openrouter.mjs:182). The main
		// submit path in handler.go already does this;
		// the rerun path missed it in PR #135 and
		// every dashboard requeue has been silently
		// broken since.
		OpenRouterModel: h.cfg.OpenRouterModel,
		// Dashboard URL/Token: same as the original
		// Job so the rerun's telemetry lands in the
		// same data layer.
		DashboardURL:   "http://boop-receiver.dev-tools.svc.cluster.local:8080",
		DashboardToken: h.cfg.RunnerToken,
		// QUB-110: parent-run id threaded into the
		// Job. The runner reads BOOP_PARENT_RUN_ID
		// when populating the prompt's PRIOR_RUN_CONTEXT
		// block; today's runner ignores the env var
		// (the prompt-block PR is a follow-up).
		TriggeredBy: "",
	})
	if err != nil {
		return newName, fmt.Errorf("build rerun job: %w", err)
	}
	// Add the parent run id as a Job annotation
	// (visible to `kubectl describe`) and an env
	// var (visible to the runner). Both copies
	// make it easy to debug a "why is this Job
	// different?" question from either side.
	//
	// Also override the Job name to the rerun
	// convention ({original}-r{N}). buildJob derives
	// the name from owner/repo/pr/sha, which is the
	// same shape for the original and every re-run
	// — the -r{N} suffix has to land here in
	// CreateRerunJob, not in buildJob, because the
	// jobbuilder is shared with the webhook path
	// that always wants the original name.
	if job.Spec.Template.ObjectMeta.Annotations == nil {
		job.Spec.Template.ObjectMeta.Annotations = map[string]string{}
	}
	job.Spec.Template.ObjectMeta.Annotations["boop/parent-run-id"] = prior.ID
	job.ObjectMeta.Name = newName
	for i, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "BOOP_JOB_NAME" {
			job.Spec.Template.Spec.Containers[0].Env[i].Value = newName
		}
	}
	job.Spec.Template.Spec.Containers[0].Env = append(
		job.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BOOP_PARENT_RUN_ID", Value: prior.ID},
	)
	if err := h.createJob(ctx, job); err != nil {
		return newName, fmt.Errorf("create rerun job: %w", err)
	}
	h.logger.Info("rerun job created", "prior", prior.ID, "new", newName, "reason", reason)
	return newName, nil
}

// buildRerunJobName is a thin shim over store.BuildRerunJobName
// (RD-002: the canonical helper lives in the store package so
// the regex / format lives in one place; the webhook package
// re-exports it for callers that already import webhook).
func buildRerunJobName(owner, repo string, pr int, sha7 string, n int) string {
	return store.BuildRerunJobName(owner, repo, pr, sha7, n)
}
