package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// errPodLogsNotConfigured is the sentinel fetchLogs
// returns when no K8s fetcher is wired. Keeping it as
// a package-level var (vs. a literal) so the "logs
// unavailable" path in templates can match it.
var errPodLogsNotConfigured = errors.New("pod log fetcher not configured")

// views_runs.go: the run-list and run-detail views.
// DP-006 split the run-shape handlers from the rest of
// dashboard.go so a contributor reading "what does the
// runs page render?" only reads this file.

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
	// SP-006: bulk fetch (runs + telemetry in one batch)
	// replaces the prior N+1 GetTelemetry loop. The
	// dashboard's HTML table renders one row per run
	// with the cost column; the join is the load-bearing
	// shape.
	page, err := h.store.ListRunsWithTelemetry(r.Context(), f)
	if err != nil {
		h.logger.Warn("dashboard runs", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	rows := make([]RunsRow, 0, len(page.Runs))
	for _, rt := range page.Runs {
		rows = append(rows, newRunsRow(rt.Run, rt.Telemetry))
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
	Runs   []RunsRow
	Filter runsFilter
}
type runsFilter struct {
	Q            string
	Status       string
	FailureClass string
}

// serveRunDetail is the /dashboard/runs/{id} view.
// DP-007: the prior shape did 4 separate store reads
// (GetRun, ListRunStages, ListLensTelemetry,
// WalkLineage). Each was a round-trip to SQLite; the
// runs page is the dashboard's hottest path and the
// detail view is opened on every run. They are now
// fanned out in parallel via errgroup-style — three
// independent reads run concurrently, the critical
// GetRun is awaited first, and the rest are best-effort
// (their results feed secondary panels). The fan-out
// is local (same process, same *sql.DB pool) so the
// cost is one open + three round-trips rather than
// three serial round-trips.
func (h *Handler) serveRunDetail(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	run, err := h.store.GetRun(ctx, id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	type stagesResult struct {
		stages  []store.RunStage
		lenses  []store.LensTelemetry
		lineage store.Lineage
	}
	resCh := make(chan stagesResult, 1)
	go func() {
		stages, _ := h.store.ListRunStages(ctx, id)
		lenses, _ := h.store.ListLensTelemetry(ctx, id)
		lineage, _ := h.store.WalkLineage(ctx, id, 32)
		resCh <- stagesResult{stages, lenses, lineage}
	}()
	res := <-resCh

	// K8s pod logs (best-effort). The Job is GC'd 1h
	// after CompletionTime, so for older runs the
	// callback returns "" and the dashboard renders
	// "logs unavailable" rather than failing the page.
	// Any error from the callback is logged at Debug
	// and treated as no-logs-available; the page still
	// renders with the structured stages + error string.
	logs, logsErr := h.fetchLogs(ctx, id)

	data := runDetailView{
		Nav:     "runs",
		Run:     newRunView(run),
		Stages:  renderWaterfall(res.stages),
		Lenses:  res.lenses,
		Lineage: res.lineage,
		Logs:    logs,
	}
	if logsErr != nil {
		data.LogsErr = logsErr.Error()
	}
	h.renderPage(w, "run_detail.html", data)
}

// fetchLogs wraps the Actions.FetchPodLogs callback so the
// handler doesn't have to nil-check it on every render. A
// nil callback (a read-only deploy without K8s) renders the
// "logs unavailable" path the same way a TTL'd Job does.
func (h *Handler) fetchLogs(ctx context.Context, jobName string) (string, error) {
	if h.actions.FetchPodLogs == nil {
		return "", errPodLogsNotConfigured
	}
	return h.actions.FetchPodLogs(ctx, jobName)
}

type runDetailView struct {
	Nav     string
	Run     RunView
	Stages  []stageRow
	Lenses  []store.LensTelemetry
	Lineage store.Lineage
	Logs    string
	LogsErr string
}
type stageRow struct {
	Stage     string
	Duration  int64
	OffsetPct float64
	WidthPct  float64
	Meta      string
	StartedAt string
	EndedAt   string
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
			out[i] = fillStageRow(stageRow{
				Stage:     s.Stage,
				Duration:  durMS(s),
				OffsetPct: float64(i) * w,
				WidthPct:  w,
			}, s)
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
		out = append(out, fillStageRow(stageRow{
			Stage:     s.Stage,
			Duration:  durMS(s),
			OffsetPct: offset,
			WidthPct:  width,
		}, s))
	}
	return out
}

// fillStageRow decorates a stageRow with the timestamps and
// Meta blob the run-detail template renders next to each
// stage bar. Kept as a helper so renderWaterfall's two
// branches stay symmetric.
func fillStageRow(r stageRow, s store.RunStage) stageRow {
	r.StartedAt = s.StartedAt.Format("15:04:05.000")
	if s.EndedAt != nil {
		r.EndedAt = s.EndedAt.Format("15:04:05.000")
	}
	r.Meta = s.Meta
	return r
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
