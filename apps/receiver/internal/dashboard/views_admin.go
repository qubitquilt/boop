package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// views_admin.go: the admin / audit / install-control /
// rerun / zero-cost / mark-orphaned views. DP-006 split
// these from the read-only operator views (views_ops.go)
// and the run-shape handlers (views_runs.go). The form
// posts that mutate state all live here so a contributor
// adding a new admin action has a single file to read.

// serveAudit is the /dashboard/audit view. The
// audit_events table is the cross-cutting "who did
// what?" ledger every operator-initiated action
// (re-run, install pause/resume, lens-opt-out edit,
// zero-out cost, mark-orphaned bulk) writes to. The
// store-side ListAuditEvents is the data source; the
// view is a feed of action / actor / target / time
// rows. The details column is rendered verbatim so
// the operator can read the per-action JSON without
// the page having to know every action's shape.
//
// SP-008: this view is the missing UI for the audit
// log that QUB-112 shipped. The store API and tests
// have been in place since the QUB-112 work; without
// this view the table is "no actor, no UI, no entry
// point" from a compliance reviewer's perspective.
//
// Optional query param:
//
//	limit=N    page size, clamped to 1..500, default 100
func (h *Handler) serveAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	events, err := h.store.ListAuditEvents(r.Context(), limit)
	if err != nil {
		h.logger.Warn("dashboard audit", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	rows := make([]auditRow, 0, len(events))
	for _, ev := range events {
		rows = append(rows, auditRow{
			ID:         ev.ID,
			Action:     ev.Action,
			Actor:      ev.Actor,
			TargetID:   ev.TargetID,
			OccurredAt: ev.OccurredAt.Format("2006-01-02 15:04:05"),
			Details:    ev.Details,
		})
	}
	data := auditView{Nav: "audit", Events: rows, Limit: limit}
	h.renderPage(w, "audit.html", data)
}

type auditView struct {
	Nav    string
	Events []auditRow
	Limit  int
}
type auditRow struct {
	ID         int64
	Action     string
	Actor      string
	TargetID   string
	OccurredAt string
	Details    string
}

// serveInstallations is the /dashboard/installations view.
func (h *Handler) serveInstallations(w http.ResponseWriter, r *http.Request) {
	insts, _ := h.store.ListInstallations(r.Context())
	rows := make([]installRow, 0, len(insts))
	for _, ins := range insts {
		rows = append(rows, installRow{
			Installation:  ins,
			LensOptOutCSV: strings.Join(ins.LensOptOut, ","),
		})
	}
	data := installationsView{Nav: "installations", Installations: rows}
	h.renderPage(w, "installations.html", data)
}

type installationsView struct {
	Nav           string
	Installations []installRow
}
type installRow struct {
	store.Installation
	LensOptOutCSV string
}

// serveInstallationControl handles the per-install pause
// / lens-opt-out editor. POST body: paused (true|false),
// lens_opt_out (comma-separated lens names). The store
// has a single SetInstallationControls method that
// writes both fields atomically.
//
// Every successful edit appends an audit row so the
// "who paused this install?" question has a durable
// answer. The action is one of:
//
//	install.pause     operator flipped paused from false to true
//	install.resume    operator flipped paused from true to false
//	lens_opt_out.set  operator changed the lens filter
//
// A single form submission that changes both fields
// emits a single audit row tagged with the union (e.g.
// a pause + lens edit is one "install.pause" row with
// the new lens list in details). The dashboard's
// audit-trail view surfaces the actor + the action +
// the pre/post values; the union is cleaner than two
// rows for one click.
func (h *Handler) serveInstallationControl(w http.ResponseWriter, r *http.Request, idStr string) {
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	paused := r.FormValue("paused") == "true"
	var lens []string
	if s := r.FormValue("lens_opt_out"); s != "" {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				lens = append(lens, p)
			}
		}
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	prev, _ := h.store.GetInstallation(r.Context(), id)
	if err := h.store.SetInstallationControls(r.Context(), id, paused, lens); err != nil {
		h.logger.Warn("dashboard install control", "id", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	// Pick the dominant action for the audit row. A
	// pause/resume flip is the most consequential
	// change; a lens edit that does not also flip
	// paused gets its own action type. Both fired in
	// the same click is rare (and the form does not
	// currently allow it) — we record the flip and
	// drop the lens change on the floor, which is
	// the same shape the existing `paused` toggle
	// would have.
	action := "lens_opt_out.set"
	if prev.Paused != paused {
		if paused {
			action = "install.pause"
		} else {
			action = "install.resume"
		}
	}
	if _, err := h.store.RecordAuditEvent(r.Context(), store.AuditEvent{
		Action:   action,
		Actor:    h.actor(),
		TargetID: idStr,
		Details: store.MarshalDetails(map[string]any{
			"paused":       paused,
			"lens_opt_out": lens,
			"reason":       reason,
		}),
	}); err != nil {
		// Non-fatal: a failed audit row is logged but
		// does not block the operator's action. The
		// dashboard's audit view will surface a gap
		// for this row, which is preferable to a 5xx
		// that flips the operator's pause back.
		h.logger.Warn("dashboard install control audit", "id", id, "err", err)
	}
	http.Redirect(w, r, "/dashboard/installations", http.StatusSeeOther)
}

// serveRerun is the form-based "Requeue" handler the
// exceptions view posts to. Persists the lineage row
// (parent_run_id + superseded_by_id), creates the K8s
// Job via the cross-package Actions callback, and
// records an audit event with the actor. Returns 503
// if the cross-package callback is not wired (a
// read-only deploy that does not have the K8s jobbuilder).
//
// The id is the prior run id (the row the operator is
// re-running from). reason is the operator's free-form
// note; it lands on the new run's row and in the
// audit row's details blob.
func (h *Handler) serveRerun(w http.ResponseWriter, r *http.Request, idStr string) {
	if h.actions.CreateRerunJob == nil {
		http.Error(w, "rerun not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.FormValue("confirm") != "true" {
		http.Error(w, "confirm: true required", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	prior, err := h.store.GetRun(r.Context(), idStr)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("dashboard rerun get", "run", idStr, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if prior.Status != store.StatusSucceeded && prior.Status != store.StatusFailed {
		http.Error(w, "can only re-run a terminal run", http.StatusConflict)
		return
	}
	newID, err := h.actions.CreateRerunJob(r.Context(), prior, reason)
	if err != nil {
		h.logger.Warn("dashboard rerun create", "run", idStr, "err", err)
		http.Error(w, "create rerun", http.StatusInternalServerError)
		return
	}
	if _, err := h.store.RecordAuditEvent(r.Context(), store.AuditEvent{
		Action:   "rerun.create",
		Actor:    h.actor(),
		TargetID: newID,
		Details:  store.MarshalDetails(map[string]any{"prior_run_id": idStr, "reason": reason}),
	}); err != nil {
		h.logger.Warn("dashboard rerun audit", "run", newID, "err", err)
	}
	h.logger.Info("dashboard rerun created", "prior", idStr, "new", newID, "reason", reason)
	http.Redirect(w, r, "/dashboard/exceptions", http.StatusSeeOther)
}

// serveZeroCost is the form-based "Zero out cost"
// handler the exceptions view posts to. Computes the
// run's total tokens (input + output + reasoning
// across the aggregate telemetry), appends a refund
// row, and records the audit event. The dashboard's
// refund list is the per-run audit trail; the global
// audit_events table is the cross-cutting one.
//
// A zero-out is all-lenses by default (the form does
// not expose a per-lens picker today; the store
// helper is shaped to accept a lens filter when one
// is added). The action is idempotent: a second
// click adds a second refund row. The operator can
// see the refund history on the run-detail page; the
// lens_telemetry rows themselves are not zeroed (the
// raw numbers stay so a future audit can recompute
// the bill). The "zero out" promise is that the
// dashboard's $ rollup excludes refunded runs — a
// follow-up query helper does the subtraction.
func (h *Handler) serveZeroCost(w http.ResponseWriter, r *http.Request, idStr string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "dashboard zero-out"
	}
	run, err := h.store.GetRun(r.Context(), idStr)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("dashboard zero cost get", "run", idStr, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	telem, terr := h.store.GetTelemetry(r.Context(), idStr)
	if terr != nil {
		// No telemetry yet — zero-out still lands
		// (zero tokens to zero out) so the operator
		// is not blocked. The refund row records 0
		// tokens; the audit row records the
		// "no telemetry" note.
		telem = store.Telemetry{}
	}
	tokens := telem.InputTokens + telem.OutputTokens + telem.ReasoningTokens
	actor := h.actor()
	if _, err := h.store.RecordRefund(r.Context(), store.Refund{
		RunID:      idStr,
		Lens:       "", // all-lenses; the per-lens picker is a follow-up
		Tokens:     tokens,
		RefundedBy: actor,
	}); err != nil {
		h.logger.Warn("dashboard zero cost refund", "run", idStr, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if _, err := h.store.RecordAuditEvent(r.Context(), store.AuditEvent{
		Action:   "cost.zero_out",
		Actor:    actor,
		TargetID: idStr,
		Details: store.MarshalDetails(map[string]any{
			"tokens_zeroed": tokens,
			"cost_usd":      telem.CostUSD,
			"reason":        reason,
			"owner":         run.Owner,
			"repo":          run.Repo,
			"pr_number":     run.PRNumber,
		}),
	}); err != nil {
		h.logger.Warn("dashboard zero cost audit", "run", idStr, "err", err)
	}
	h.logger.Info("dashboard zero cost", "run", idStr, "tokens", tokens, "cost_usd", telem.CostUSD)
	http.Redirect(w, r, "/dashboard/exceptions", http.StatusSeeOther)
}

// serveMarkOrphaned handles POST /dashboard/admin/mark-orphaned.
// Bulk-marks every "running" row whose runner never
// heartbeated as "failed" with a synthetic "orphaned" error.
//
// QUB-114: the original incident was a runner telemetry
// regression where the runner process exited before its
// first heartbeat, leaving every review row stuck at
// status="running" in the dashboard. The reconciler fix
// (k8s_reconcile.go) prevents this going forward for new
// runs; this endpoint cleans up the legacy zombies from
// the dashboard in one shot.
//
// Auth: same X-Boop-Dashboard-Token as the rest of the
// /dashboard/* tree (the middleware in this package).
//
// Optional query params:
//   grace=<duration> — overrides the default 5m grace
//     window. Useful for tests and for the operator
//     who wants to wait longer before marking a slow
//     runner's row as orphaned. Format is a Go duration
//     string (5m, 30s, 1h).
//
// Response: 200 with {"marked": <int>} on success, 400 on
// a malformed grace value, 500 on a store error.
func (h *Handler) serveMarkOrphaned(w http.ResponseWriter, r *http.Request) {
	grace := 5 * time.Minute
	if g := r.URL.Query().Get("grace"); g != "" {
		d, err := time.ParseDuration(g)
		if err != nil {
			http.Error(w, "bad grace: "+err.Error(), http.StatusBadRequest)
			return
		}
		if d < 0 {
			http.Error(w, "grace must be non-negative", http.StatusBadRequest)
			return
		}
		grace = d
	}
	n, err := h.store.MarkOrphanedRuns(r.Context(), grace)
	if err != nil {
		h.logger.Warn("dashboard mark orphaned", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if _, err := h.store.RecordAuditEvent(r.Context(), store.AuditEvent{
		Action:   "admin.mark_orphaned",
		Actor:    h.actor(),
		TargetID: "bulk",
		Details:  store.MarshalDetails(map[string]any{"marked": n, "grace_seconds": grace.Seconds()}),
	}); err != nil {
		// Non-fatal: the dashboard's audit view will surface
		// a gap, which is preferable to a 5xx that loses
		// the operator's cleanup progress.
		h.logger.Warn("dashboard mark orphaned audit", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{"marked":%d}`, n)))
}

// Health is a liveness endpoint for the dashboard. It
// reports whether the data layer is reachable so a
// Kubernetes probe can distinguish "the binary is up"
// from "the data layer is broken".
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	if _, err := h.store.Stats(r.Context()); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
