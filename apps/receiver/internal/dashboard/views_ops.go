package dashboard

import (
	"net/http"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// views_ops.go: the live / exceptions / costs / retention
// views. DP-006 split the read-only operator views from
// the run-shape handlers (views_runs.go) and the
// admin-action handlers (views_admin.go). Each file
// owns one slice of the dashboard's surface.

// serveLive is the /dashboard/live view.
func (h *Handler) serveLive(w http.ResponseWriter, r *http.Request) {
	running, _ := h.store.ListRuns(r.Context(), store.ListRunsFilter{Status: store.StatusRunning, Limit: 100})
	stuck, _ := h.store.ListStuckRuns(r.Context(), 2*time.Minute, 100)
	runRows := make([]LiveRow, 0, len(running.Runs))
	for _, run := range running.Runs {
		runRows = append(runRows, newLiveRow(run))
	}
	stuckRows := make([]LiveRow, 0, len(stuck))
	for _, run := range stuck {
		stuckRows = append(stuckRows, newLiveRow(run))
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
	Running []LiveRow
	Stuck   []LiveRow
}

// serveExceptions is the /dashboard/exceptions view.
func (h *Handler) serveExceptions(w http.ResponseWriter, r *http.Request) {
	class := r.URL.Query().Get("class")
	all, _ := h.store.ListRuns(r.Context(), store.ListRunsFilter{Status: store.StatusFailed, Limit: 200})
	rows := make([]ExceptionRow, 0)
	for _, run := range all.Runs {
		if run.FailureClass == "" {
			continue
		}
		if class != "" && run.FailureClass != class {
			continue
		}
		rows = append(rows, newExceptionRow(run))
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
	Exceptions []ExceptionRow
	Filter     exceptionsFilter
}
type exceptionsFilter struct {
	Class string
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
