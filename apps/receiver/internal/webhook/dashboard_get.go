package webhook

// Dashboard GET endpoints (RF-007 split).
//
// Read-only views for the dashboard. All filters are
// optional; missing filters mean "no constraint". The
// query string keys match the dashboard's URL shape
// (from, to, owner, repo, status, installation, cursor,
// limit). The auth model for the GETs is "open within
// the cluster" — the dashboard service is internal; if
// the dashboard moves behind an Ingress, the auth layer
// sits in front of the dashboard, not in the receiver.

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"github.com/michaelruelas/boop-receiver/internal/store"
)

// InstallationsResponse is the body of GET /api/installations.
// The dashboard renders one row per installation; the count is
// the dashboard's "X repos installed Boop" KPI.
type InstallationsResponse struct {
	Installations []boopgithub.Installation `json:"installations"`
	FetchedAt     time.Time                 `json:"fetched_at"`
}

// ListInstallations handles GET /api/installations. Returns the
// cached list from the store (refreshed every 5 min by a
// background poller started in main). If the store is empty (e.g.
// right after receiver start, before the first poll completes),
// the handler does a synchronous refresh so the dashboard
// doesn't show an empty list for the first 5 minutes.
func (h *Handler) ListInstallations(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()

	rows, err := h.store.ListInstallations(ctx)
	if err != nil {
		h.logger.Warn("list installations", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	fetchedAt, _ := h.store.LatestInstallationFetch(ctx)
	if len(rows) == 0 && h.ghClient != nil {
		// Cold start: do a synchronous refresh so the dashboard
		// doesn't have to wait up to 5 min. Errors here are
		// non-fatal — the dashboard shows an empty list and
		// the next poll will populate it.
		if fresh, ferr := h.ghClient.ListInstallations(ctx); ferr == nil {
			installs := make([]store.Installation, len(fresh))
			for i, ins := range fresh {
				installs[i] = store.Installation{
					ID:                  ins.ID,
					AccountLogin:        ins.AccountLogin,
					AccountType:         ins.AccountType,
					RepositorySelection: ins.RepositorySelection,
					InstalledAt:         ins.InstalledAt,
				}
			}
			if err := h.store.UpsertInstallations(ctx, installs); err == nil {
				rows = installs
				fetchedAt = time.Now().UTC()
			} else {
				// EH-010: a failed upsert here used to
				// be silently swallowed. The dashboard
				// would then render an empty list and the
				// operator would have no idea why. A
				// warn-level log is enough — the next
				// background poll will retry — but the
				// operator now sees the failure in the
				// receiver's log stream and can correlate
				// it with their dashboard complaint.
				h.logger.Warn("cold start upsert installations", "err", err)
			}
		} else {
			h.logger.Warn("cold start refresh installations", "err", ferr)
		}
	}

	// The store's Installation and the GitHub manager's
	// Installation are deliberately separate types — neither
	// package should import the other. Convert at the wire
	// boundary so the dashboard gets a single shape.
	out := make([]boopgithub.Installation, len(rows))
	for i, ins := range rows {
		out[i] = boopgithub.Installation{
			ID:                  ins.ID,
			AccountLogin:        ins.AccountLogin,
			AccountType:         ins.AccountType,
			RepositorySelection: ins.RepositorySelection,
			InstalledAt:         ins.InstalledAt,
		}
	}
	writeJSON(w, http.StatusOK, InstallationsResponse{
		Installations: out,
		FetchedAt:     fetchedAt,
	})
}

// ListRuns handles GET /api/runs. All filters are optional;
// missing filters mean "no constraint". The query string keys
// match the dashboard's URL shape:
//
//	from=rfc3339    inclusive lower bound on started_at
//	to=rfc3339      inclusive upper bound on started_at
//	owner=...       exact match on owner
//	repo=...        exact match on repo
//	status=...      one of pending|running|succeeded|failed
//	installation=   exact match on installation_id
//	cursor=...      keyset cursor from a previous response
//	limit=N         page size, clamped to 1..200, default 50
//
// The response includes next_cursor when more rows exist. An
// empty next_cursor means "no more pages".
func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	f := store.ListRunsFilter{
		Owner:  q.Get("owner"),
		Repo:   q.Get("repo"),
		Cursor: q.Get("cursor"),
	}
	if s := q.Get("status"); s != "" {
		f.Status = store.RunStatus(s)
	}
	if s := q.Get("installation"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			f.InstallationID = n
		}
	}
	if s := q.Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.From = t
		}
	}
	if s := q.Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.To = t
		}
	}
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Limit = n
		}
	}

	// SP-006: bulk fetch (runs + telemetry in one batch)
	// replaces the N+1 GetTelemetry loop. The dashboard's
	// run-list wants every run with its cost column, so
	// the join is the load-bearing shape.
	page, err := h.store.ListRunsWithTelemetry(r.Context(), f)
	if err != nil {
		h.logger.Warn("list runs", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]RunWithTelemetry, 0, len(page.Runs))
	for _, rt := range page.Runs {
		out = append(out, RunWithTelemetry{Run: rt.Run, Telemetry: rt.Telemetry})
	}
	writeJSON(w, http.StatusOK, ListRunsResponse{
		Runs:       out,
		NextCursor: page.NextCursor,
	})
}

// RunWithTelemetry pairs a Run with its Telemetry, if any. The
// dashboard's runs table shows cost + tokens on each row, and a
// separate API call would mean a join on the client side. Cheap
// to assemble here.
type RunWithTelemetry struct {
	store.Run
	Telemetry store.Telemetry `json:"telemetry,omitempty"`
}

// ListRunsResponse is the body of GET /api/runs.
type ListRunsResponse struct {
	Runs       []RunWithTelemetry `json:"runs"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// GetRun handles GET /api/runs/{id}. Returns a single run with its
// telemetry, or 404 if the run does not exist.
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	run, err := h.store.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrUnknownRun) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.logger.Warn("get run", "id", id, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	var telem store.Telemetry
	if t, err := h.store.GetTelemetry(ctx, id); err == nil {
		telem = t
	}
	writeJSON(w, http.StatusOK, RunWithTelemetry{Run: run, Telemetry: telem})
}

// StatsResponse is the body of GET /api/stats. The shape is
// stable: summary is the top-line KPI block, buckets is the
// time-series, by_repo and by_model are the leaderboards. The
// dashboard renders this in one fetch.
type StatsResponse struct {
	From    time.Time           `json:"from"`
	To      time.Time           `json:"to"`
	Bucket  store.StatsBucket   `json:"bucket"`
	Summary store.SummaryStats  `json:"summary"`
	Buckets []store.BucketPoint `json:"buckets"`
	ByRepo  []store.RepoRollup  `json:"by_repo"`
	ByModel []store.ModelRollup `json:"by_model"`
}

// Stats handles GET /api/stats. Defaults: 30-day window ending
// now, day-bucketed series, top 50 repos / all models. The
// dashboard can override any of these via query string.
func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "data layer disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	from := now.Add(-30 * 24 * time.Hour)
	to := now
	if s := q.Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		}
	}
	if s := q.Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		}
	}
	if to.Before(from) {
		http.Error(w, "to < from", http.StatusBadRequest)
		return
	}
	bucket := store.BucketDay
	if s := q.Get("bucket"); s != "" {
		bucket = store.StatsBucket(s)
		switch bucket {
		case store.BucketHour, store.BucketDay, store.BucketWeek:
		default:
			http.Error(w, "invalid bucket", http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	summary, err := h.store.Summary(ctx, from, to)
	if err != nil {
		h.logger.Warn("stats summary", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	buckets, err := h.store.BucketSeries(ctx, from, to, bucket)
	if err != nil {
		h.logger.Warn("stats buckets", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	byRepo, err := h.store.PerRepo(ctx, from, to, 50)
	if err != nil {
		h.logger.Warn("stats per repo", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	byModel, err := h.store.PerModel(ctx, from, to)
	if err != nil {
		h.logger.Warn("stats per model", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, StatsResponse{
		From:    from,
		To:      to,
		Bucket:  bucket,
		Summary: summary,
		Buckets: buckets,
		ByRepo:  byRepo,
		ByModel: byModel,
	})
}
