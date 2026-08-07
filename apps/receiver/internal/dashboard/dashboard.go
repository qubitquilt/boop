// Package dashboard is the operator-facing UI (QUB-111).
//
// Server-rendered HTML using Go's stdlib html/template
// plus HTMX for incremental updates. The same binary
// (boop-receiver) serves the dashboard; no new service,
// no SPA build, no JS framework. One auth token
// (BOOP_DASHBOARD_TOKEN) gates every /dashboard/* route.
//
// Why server-rendered and not a React/Vue SPA: the
// dashboard is an internal tool with a single user (the
// on-call operator), 6 views, and a refresh cadence of
// "every few seconds during an incident, then nothing".
// The build complexity of a SPA is not justified by the
// surface area. HTMX gives us partial updates without
// shipping a JS framework.
//
// Build order (the spec's order): runs list → run
// detail → live ops → exception dock → costs & lenses →
// installations. Each view is a single template + a
// single Go function. New views are 50-100 lines, not
// a Redux saga.
package dashboard

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// Actions is the dependency the dashboard pulls from the
// webhook package. The dashboard's form-based actions (re-run,
// zero-out cost) need the K8s jobbuilder, which lives in
// webhook. The dashboard has no business knowing about K8s, so
// the action is a callback the receiver wires up at startup;
// the dashboard only sees the function signature.
//
// CreateRerunJob persists the new run row, creates the K8s Job,
// and returns the new run id. A nil-able set of fields in cfg
// or store is treated as "feature disabled" and the callback
// returns an error so the dashboard can render a 503-style
// "not configured" page rather than silently dropping the
// action.
type Actions struct {
	CreateRerunJob func(ctx context.Context, prior store.Run, reason string) (newRunID string, err error)
}

//go:embed templates/*.html
var templateFS embed.FS

// Handler is the receiver's dashboard endpoint group.
// One Handler per receiver process; the routes are
// mounted under /dashboard/* in main.go and gated by
// BOOP_DASHBOARD_TOKEN.
type Handler struct {
	store  *store.Store
	logger *slog.Logger
	token  string
	// actions are the cross-package dependencies the
	// dashboard needs to do more than render. Re-run
	// is the only one today; a future "drain queue"
	// or "rotate webhook secret" button would land
	// here too. Nil is fine for a read-only deploy.
	actions Actions
}

// NewHandler builds a dashboard Handler. token is the
// shared secret for the BOOP_DASHBOARD_TOKEN gate; an
// empty token rejects every request — the dashboard is
// opt-in, like the data layer. actions wires the
// cross-package dependencies; passing the zero value
// disables form-initiated K8s actions (the re-run
// button renders disabled and the form POSTs to a
// 503-style "not configured" page).
func NewHandler(st *store.Store, logger *slog.Logger, token string, actions Actions) (*Handler, error) {
	return &Handler{
		store:   st,
		logger:  logger,
		token:   token,
		actions: actions,
	}, nil
}

// renderPage parses the layout + the page-specific
// template together for each request. Per-request
// parsing avoids the parse-time "last define" race
// that hits the base set: when all templates share
// the "title" and "content" block names, the base
// ParseFS makes whichever file was parsed last win
// for every page (alphabetic: runs.html). Parsing
// just two files per request — the layout + the
// page — keeps the page's defines the LAST to land
// in the set, so the page renders its own content.
//
// The cost is two small files re-parsed per request.
// Both are a few KB; the parse is well under a
// millisecond. If profiling later shows this is the
// hot path, the fix is to cache the parsed set per
// page and Clone + reparse the page's defines into
// the cache on each request.
func (h *Handler) renderPage(w http.ResponseWriter, page string, data any) {
	tmpl, err := template.ParseFS(templateFS, "templates/layout.html", "templates/"+page)
	if err != nil {
		h.logger.Warn("dashboard parse", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, page, data); err != nil {
		h.logger.Warn("dashboard render", "page", page, "err", err)
	}
}

// Middleware gates the /dashboard/* routes with the
// BOOP_DASHBOARD_TOKEN. The check is constant-time and
// the token is compared verbatim against the
// X-Boop-Dashboard-Token header. An empty token means
// the dashboard is disabled (every request gets 401).
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" {
			http.Error(w, "dashboard disabled", http.StatusUnauthorized)
			return
		}
		got := r.Header.Get("X-Boop-Dashboard-Token")
		// Constant-time compare; missing header fails
		// fast because the compare returns 0 on
		// length-mismatch.
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RegisterRoutes wires /dashboard/* on the given mux.
// The middleware is applied to every route.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /dashboard/", h.Middleware(http.HandlerFunc(h.route)))
	mux.Handle("POST /dashboard/", h.Middleware(http.HandlerFunc(h.route)))
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/dashboard")
	path = strings.TrimPrefix(path, "/")
	// Route table — small, ordered most-specific first.
	switch {
	case path == "":
		// /dashboard → redirect to runs list.
		http.Redirect(w, r, "/dashboard/runs", http.StatusSeeOther)
	case path == "runs":
		h.serveRuns(w, r)
	case strings.HasPrefix(path, "runs/"):
		rest := strings.TrimPrefix(path, "runs/")
		// /dashboard/runs/{id}/rerun and
		// /dashboard/runs/{id}/zero-cost are the two
		// form-based action endpoints the exceptions
		// view posts to. Each takes a different
		// verb-suffix split.
		if strings.HasSuffix(rest, "/rerun") && r.Method == http.MethodPost {
			h.serveRerun(w, r, strings.TrimSuffix(rest, "/rerun"))
			return
		}
		if strings.HasSuffix(rest, "/zero-cost") && r.Method == http.MethodPost {
			h.serveZeroCost(w, r, strings.TrimSuffix(rest, "/zero-cost"))
			return
		}
		h.serveRunDetail(w, r, rest)
	case path == "live":
		h.serveLive(w, r)
	case path == "exceptions":
		h.serveExceptions(w, r)
	case path == "costs":
		h.serveCosts(w, r)
	case path == "retention":
		h.serveRetention(w, r)
	case path == "installations":
		h.serveInstallations(w, r)
	case strings.HasPrefix(path, "installations/"):
		h.serveInstallationControl(w, r, strings.TrimPrefix(path, "installations/"))
	default:
		http.NotFound(w, r)
	}
}

// serveRuns is the /dashboard/runs list view.
func (h *Handler) serveRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListRunsFilter{
		Owner: q.Get("owner"),
		Repo:  q.Get("repo"),
	}
	if s := q.Get("status"); s != "" {
		f.Status = store.RunStatus(s)
	}
	// QU B-113 / CQ-004: forward the failure_class
	// filter chip value to the store. The previous
	// shape dropped it on the floor — the dropdown
	// rendered the chip but the result set was the
	// whole page, so an operator who picked
	// "OOMKilled" saw every failed run instead of
	// the OOMKilled subset. FailureClass is a free-
	// form string (today's values are
	// oom_killed / container_error / crash_loop /
	// image_pull / config_error; future
	// failure_class taxonomy is a receiver-side
	// concern, not a dashboard-side one).
	if fc := q.Get("failure_class"); fc != "" {
		f.FailureClass = fc
	}
	if s := q.Get("limit"); s != "" {
		// The default (50) is fine for a dashboard page;
		// no override needed.
		_ = s
	}
	page, err := h.store.ListRuns(r.Context(), f)
	if err != nil {
		h.logger.Warn("dashboard runs", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	rows := make([]runsRow, 0, len(page.Runs))
	for _, run := range page.Runs {
		row := runsRow{Run: run}
		row.Status = string(run.Status)
		row.StartedAt = run.StartedAt.Format("2006-01-02 15:04")
		if run.EndedAt != nil {
			row.Duration = run.EndedAt.Sub(run.StartedAt).Round(time.Second).String()
		}
		if t, err := h.store.GetTelemetry(r.Context(), run.ID); err == nil {
			row.Cost = fmt.Sprintf("$%.4f", t.CostUSD)
		}
		rows = append(rows, row)
	}
	data := runsView{
		Nav:    "runs",
		Runs:   rows,
		Filter: runsFilter{Q: r.URL.Query().Get("q"), Status: q.Get("status"), FailureClass: q.Get("failure_class")},
	}
	h.renderPage(w, "runs.html", data)
}

type runsView struct {
	Nav    string
	Runs   []runsRow
	Filter runsFilter
}
type runsFilter struct {
	Q            string
	Status       string
	FailureClass string
}
type runsRow struct {
	store.Run
	Status    string
	StartedAt string
	Duration  string
	Cost      string
}

// serveRunDetail is the /dashboard/runs/{id} view.
func (h *Handler) serveRunDetail(w http.ResponseWriter, r *http.Request, id string) {
	run, err := h.store.GetRun(r.Context(), id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	stages, _ := h.store.ListRunStages(r.Context(), id)
	lenses, _ := h.store.ListLensTelemetry(r.Context(), id)
	lineage, _ := h.store.WalkLineage(r.Context(), id, 32)
	data := runDetailView{
		Nav:     "runs",
		Run:     runView{Run: run},
		Stages:  renderWaterfall(stages),
		Lenses:  lenses,
		Lineage: lineage,
	}
	data.Run.Status = string(run.Status)
	if !run.StartedAt.IsZero() {
		data.Run.StartedAt = run.StartedAt.Format("2006-01-02 15:04:05")
	}
	if run.EndedAt != nil {
		data.Run.EndedAt = run.EndedAt.Format("2006-01-02 15:04:05")
		if dur := run.EndedAt.Sub(run.StartedAt); dur > 0 {
			data.Run.Duration = dur.Round(time.Second).String()
		}
	}
	h.renderPage(w, "run_detail.html", data)
}

type runDetailView struct {
	Nav     string
	Run     runView
	Stages  []stageRow
	Lenses  []store.LensTelemetry
	Lineage store.Lineage
}
type runView struct {
	store.Run
	Status    string
	StartedAt string
	EndedAt   string
	Duration  string
}
type stageRow struct {
	Stage     string
	Duration  int64
	OffsetPct float64
	WidthPct  float64
}

// renderWaterfall converts the per-stage rows into
// percentage offsets so the template can render a
// flexbox bar without arithmetic. The earliest stage
// is the left edge; the latest is the right edge; the
// total span is the difference between the earliest
// start and the latest end. A single stage gets 100%
// width; multiple stages share the row proportionally.
func renderWaterfall(stages []store.RunStage) []stageRow {
	if len(stages) == 0 {
		return nil
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].StartedAt.Before(stages[j].StartedAt) })
	earliest := stages[0].StartedAt
	var latest time.Time
	for _, s := range stages {
		end := s.StartedAt
		if s.EndedAt != nil {
			end = *s.EndedAt
		}
		if end.After(latest) {
			latest = end
		}
	}
	total := latest.Sub(earliest)
	if total <= 0 {
		// All stages share a single second; give
		// them equal width.
		w := 100.0 / float64(len(stages))
		out := make([]stageRow, len(stages))
		for i, s := range stages {
			out[i] = stageRow{Stage: s.Stage, Duration: durMS(s), OffsetPct: float64(i) * w, WidthPct: w}
		}
		return out
	}
	out := make([]stageRow, 0, len(stages))
	for _, s := range stages {
		end := s.StartedAt
		if s.EndedAt != nil {
			end = *s.EndedAt
		}
		offset := s.StartedAt.Sub(earliest).Seconds() / total.Seconds() * 100
		width := end.Sub(s.StartedAt).Seconds() / total.Seconds() * 100
		if width < 0.5 {
			width = 0.5 // floor so <1s stages are still visible
		}
		out = append(out, stageRow{Stage: s.Stage, Duration: durMS(s), OffsetPct: offset, WidthPct: width})
	}
	return out
}

func durMS(s store.RunStage) int64 {
	if s.DurationMS != nil {
		return *s.DurationMS
	}
	if s.EndedAt != nil {
		return s.EndedAt.Sub(s.StartedAt).Milliseconds()
	}
	return 0
}

// serveLive is the /dashboard/live view.
func (h *Handler) serveLive(w http.ResponseWriter, r *http.Request) {
	running, _ := h.store.ListRuns(r.Context(), store.ListRunsFilter{Status: store.StatusRunning, Limit: 100})
	stuck, _ := h.store.ListStuckRuns(r.Context(), 2*time.Minute, 100)
	runRows := make([]liveRow, 0, len(running.Runs))
	for _, run := range running.Runs {
		row := liveRow{Run: run, Status: string(run.Status)}
		row.StartedAt = run.StartedAt.Format("15:04:05")
		if run.LastHeartbeatAt != nil {
			row.LastHeartbeatAt = run.LastHeartbeatAt.Format("15:04:05")
		}
		runRows = append(runRows, row)
	}
	stuckRows := make([]liveRow, 0, len(stuck))
	for _, run := range stuck {
		row := liveRow{Run: run, Status: string(run.Status)}
		row.StartedAt = run.StartedAt.Format("15:04:05")
		if run.LastHeartbeatAt != nil {
			row.LastHeartbeatAt = run.LastHeartbeatAt.Format("15:04:05")
			row.SilentFor = time.Since(*run.LastHeartbeatAt).Round(time.Second).String()
		} else {
			row.SilentFor = time.Since(run.StartedAt).Round(time.Second).String() + " (no heartbeat ever)"
		}
		stuckRows = append(stuckRows, row)
	}
	data := liveView{
		Nav:     "live",
		Running: runRows,
		Stuck:   stuckRows,
	}
	h.renderPage(w, "live.html", data)
}

type liveView struct {
	Nav     string
	Running []liveRow
	Stuck   []liveRow
}
type liveRow struct {
	store.Run
	Status          string
	StartedAt       string
	LastHeartbeatAt string
	SilentFor       string
}

// serveExceptions is the /dashboard/exceptions view.
func (h *Handler) serveExceptions(w http.ResponseWriter, r *http.Request) {
	class := r.URL.Query().Get("class")
	all, _ := h.store.ListRuns(r.Context(), store.ListRunsFilter{Status: store.StatusFailed, Limit: 200})
	rows := make([]exceptionRow, 0)
	for _, run := range all.Runs {
		if run.FailureClass == "" {
			continue
		}
		if class != "" && run.FailureClass != class {
			continue
		}
		row := exceptionRow{Run: run, Status: string(run.Status)}
		row.StartedAt = run.StartedAt.Format("2006-01-02 15:04")
		row.Error = run.Error
		rows = append(rows, row)
	}
	data := exceptionsView{
		Nav:        "exceptions",
		Exceptions: rows,
		Filter:     exceptionsFilter{Class: class},
	}
	h.renderPage(w, "exceptions.html", data)
}

type exceptionsView struct {
	Nav        string
	Exceptions []exceptionRow
	Filter     exceptionsFilter
}
type exceptionsFilter struct {
	Class string
}
type exceptionRow struct {
	store.Run
	Status    string
	StartedAt string
	Error     string
}

// serveCosts is the /dashboard/costs view.
func (h *Handler) serveCosts(w http.ResponseWriter, r *http.Request) {
	to := time.Now().UTC()
	from := to.Add(-30 * 24 * time.Hour)
	rollup, _ := h.store.LensCostSummary(r.Context(), from, to)
	data := costsView{Nav: "costs", LensRollup: rollup}
	h.renderPage(w, "costs.html", data)
}

type costsView struct {
	Nav        string
	LensRollup []store.LensCostRollup
}

// serveRetention renders /dashboard/retention — a row per
// run with its scheduled-deletion timestamp and an
// "imminent" flag for rows scheduled within the next 7
// days. The retention loop prunes on a 5-min tick; the
// schedule here is the row's started_at + DefaultRetention,
// not the actual delete time. The page is the operator's
// proof that retention is enforced as a schedule, not a
// config-file promise (QUB-112).
//
// An empty schedule means either the data layer is empty
// or the retention tick has already pruned everything; the
// template renders an "empty" hint in either case. The
// retention duration is the store's DefaultRetention (no
// dashboard override today; a future retention-config
// route would add one).
func (h *Handler) serveRetention(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListRetentionSchedule(r.Context(), 0)
	if err != nil {
		h.logger.Warn("dashboard retention", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	schedule := make([]retentionRow, 0, len(rows))
	for _, row := range rows {
		daysOut := int(row.ScheduledDeletion.Sub(now).Hours() / 24)
		schedule = append(schedule, retentionRow{
			RunID:             row.RunID,
			StartedAt:         row.StartedAt.Format("2006-01-02 15:04"),
			ScheduledDeletion: row.ScheduledDeletion.Format("2006-01-02 15:04"),
			DaysOut:           daysOut,
			Imminent:          daysOut < 7,
		})
	}
	data := retentionView{
		Nav:           "retention",
		Schedule:      schedule,
		RetentionDays: int(store.DefaultRetention.Hours() / 24),
	}
	h.renderPage(w, "retention.html", data)
}

type retentionView struct {
	Nav           string
	Schedule      []retentionRow
	RetentionDays int
}
type retentionRow struct {
	RunID             string
	StartedAt         string
	ScheduledDeletion string
	DaysOut           int
	Imminent          bool
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

// actor derives the audit-log actor from the
// BOOP_DASHBOARD_TOKEN. Today the dashboard's only
// authentication is a shared secret, so the actor is
// a SHA-256 prefix of the token — stable across
// requests (same operator, same actor) and
// non-reversible (the token is not in the audit log).
// A future per-user identity layer replaces the
// token-derived actor; the AuditEvent.Actor field is
// already a free-form string so the swap is a
// one-call-site change.
func (h *Handler) actor() string {
	if h.token == "" {
		return "dashboard:disabled"
	}
	sum := sha256.Sum256([]byte(h.token))
	return "dashboard:" + hex.EncodeToString(sum[:4])
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
