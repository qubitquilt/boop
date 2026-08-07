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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

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
	sha7 := shortSHARerun(prior.CommitSHA)
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
// run row with parent_run_id set; the actual K8s Job
// creation is the responsibility of a follow-up PR that
// threads BOOP_PARENT_RUN_ID through the jobbuilder.
// Today's implementation persists the row, sets
// superseded_by_id on the prior, and returns the new id
// — Phase 3 ships the lineage half, Phase 4 wires the
// K8s jobbuilder half.
func (h *Handler) Rerun(w http.ResponseWriter, r *http.Request) {
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
	sha7 := shortSHARerun(prior.CommitSHA)
	count, err := h.store.CountRerunJobsForSHA(r.Context(), prior.Owner, prior.Repo, prior.PRNumber, sha7)
	if err != nil {
		h.logger.Warn("rerun count", "run", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	newName := buildRerunJobName(prior.Owner, prior.Repo, prior.PRNumber, sha7, count+1)
	if _, err := h.store.UpsertRun(r.Context(), store.Run{
		ID:             newName,
		Owner:          prior.Owner,
		Repo:           prior.Repo,
		PRNumber:       prior.PRNumber,
		CommitSHA:      prior.CommitSHA,
		BaseRef:        prior.BaseRef,
		ReviewNumber:   prior.ReviewNumber + 1,
		Reason:         "rerun: " + body.Reason,
		InstallationID: prior.InstallationID,
		Status:         store.StatusPending,
		StartedAt:      time.Now().UTC(),
		ParentRunID:    prior.ID,
	}); err != nil {
		h.logger.Warn("rerun upsert", "run", newName, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if err := h.store.SetSupersededBy(r.Context(), prior.ID, newName); err != nil {
		h.logger.Warn("rerun supersede", "prior", prior.ID, "new", newName, "err", err)
		// Non-fatal — the new run row already exists;
		// the lineage view will still work via
		// parent_run_id. Log and continue.
	}
	h.logger.Info("rerun created",
		"prior", prior.ID,
		"new", newName,
		"reason", body.Reason,
	)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"new_run_id":    newName,
		"prior_run_id":  prior.ID,
		"parent_run_id": prior.ID,
		"note":          "Phase 3 ships the lineage half; K8s Job creation is wired in Phase 4",
	})
}

// buildRerunJobName constructs the Job name for the
// (owner, repo, pr, sha7) tuple with a -r{n} suffix.
// n starts at 1 for the first re-run.
func buildRerunJobName(owner, repo string, pr int, sha7 string, n int) string {
	if n < 1 {
		n = 1
	}
	return fmt.Sprintf("%s-r%d", buildJobNameRerun(owner, repo, pr, sha7), n)
}

// shortSHARerun / buildJobNameRerun mirror handler.go's
// helpers. Duplicated here because handler.go's are
// unexported and the re-run flow needs them. The
// implementations match exactly.
func shortSHARerun(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

var rerunJobNameSanitizer = regexp.MustCompile(`[^a-z0-9-]`)

func buildJobNameRerun(owner, repo string, pr int, sha string) string {
	raw := fmt.Sprintf("boop-%s-%s-%d-%s", owner, repo, pr, shortSHARerun(sha))
	return rerunJobNameSanitizer.ReplaceAllString(strings.ToLower(raw), "-")
}
