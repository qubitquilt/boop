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
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Handler is the receiver's dashboard endpoint group.
// One Handler per receiver process; the routes are
// mounted under /dashboard/* in main.go and gated by
// BOOP_DASHBOARD_TOKEN.
type Handler struct {
	store     *store.Store
	logger    *slog.Logger
	token     string
	templates *template.Template
}

// NewHandler builds a dashboard Handler. token is the
// shared secret for the BOOP_DASHBOARD_TOKEN gate; an
// empty token rejects every request — the dashboard is
// opt-in, like the data layer.
func NewHandler(st *store.Store, logger *slog.Logger, token string) (*Handler, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse templates: %w", err)
	}
	return &Handler{
		store:     st,
		logger:    logger,
		token:     token,
		templates: tmpl,
	}, nil
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
		h.serveRunDetail(w, r, strings.TrimPrefix(path, "runs/"))
	case path == "live":
		h.serveLive(w, r)
	case path == "exceptions":
		h.serveExceptions(w, r)
	case path == "costs":
		h.serveCosts(w, r)
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
	if err := h.templates.ExecuteTemplate(w, "runs.html", data); err != nil {
		h.logger.Warn("dashboard runs render", "err", err)
	}
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
	if err := h.templates.ExecuteTemplate(w, "run_detail.html", data); err != nil {
		h.logger.Warn("dashboard run detail render", "err", err)
	}
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
	if err := h.templates.ExecuteTemplate(w, "live.html", data); err != nil {
		h.logger.Warn("dashboard live render", "err", err)
	}
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
	if err := h.templates.ExecuteTemplate(w, "exceptions.html", data); err != nil {
		h.logger.Warn("dashboard exceptions render", "err", err)
	}
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
	if err := h.templates.ExecuteTemplate(w, "costs.html", data); err != nil {
		h.logger.Warn("dashboard costs render", "err", err)
	}
}

type costsView struct {
	Nav        string
	LensRollup []store.LensCostRollup
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
	if err := h.templates.ExecuteTemplate(w, "installations.html", data); err != nil {
		h.logger.Warn("dashboard installations render", "err", err)
	}
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
	if err := h.store.SetInstallationControls(r.Context(), id, paused, lens); err != nil {
		h.logger.Warn("dashboard install control", "id", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/dashboard/installations", http.StatusSeeOther)
}

// renderJSON is a debug helper used during development;
// not wired to a route but kept around in case the
// operator wants to curl the dashboard data without
// the HTML shell.
func renderJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

// ensure context import survives goimports.
var _ = context.Background
