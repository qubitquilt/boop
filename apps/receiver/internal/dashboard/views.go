package dashboard

import (
	"strconv"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// This file holds the view-specific DTOs the dashboard's HTML
// templates render. They are deliberately NOT embedded
// store.Run; every field is listed explicitly so a future
// contributor adding a column to the runs table cannot
// silently leak it to the wire (DP-004). The mapping from
// store.Run to RunView lives in newRunView and is the only
// place the conversion happens; a one-call-site change
// widens or narrows the wire shape.

// RunView is the run-shaped value every dashboard view
// renders. It exposes only the fields a view actually
// uses; the store has more, but those are internal. A
// future column on store.Run is not visible to the
// dashboard until newRunView explicitly projects it,
// which is the load-bearing "no column leaks to the
// wire" rule the audit asked for.
type RunView struct {
	ID            string
	Owner         string
	Repo          string
	PRNumber      int
	Status        string
	StartedAt     string
	EndedAt       string
	Duration      string
	FailureClass  string
	Error         string
	PRURL         string
	CommitSHA     string
	ReviewNumber  int
	Reason        string
	LastHeartbeat string
	SilentFor     string
}

// newRunView projects a store.Run into the view-shaped
// one. Every field the dashboard renders passes through
// this function. A column that the store gains but the
// dashboard does not want is left out here, and the wire
// shape stays stable.
//
// The StartedAt / EndedAt / Duration / PRURL formatting
// lives here (not in the serve* functions) so the
// view layer is a single conversion point. The
// templates see only the formatted strings.
func newRunView(r store.Run) RunView {
	v := RunView{
		ID:            r.ID,
		Owner:         r.Owner,
		Repo:          r.Repo,
		PRNumber:      r.PRNumber,
		Status:        string(r.Status),
		FailureClass:  r.FailureClass,
		Error:         r.Error,
		CommitSHA:     r.CommitSHA,
		ReviewNumber:  r.ReviewNumber,
		Reason:        r.Reason,
	}
	if !r.StartedAt.IsZero() {
		v.StartedAt = r.StartedAt.Format("2006-01-02 15:04:05")
	}
	if r.EndedAt != nil {
		v.EndedAt = r.EndedAt.Format("2006-01-02 15:04:05")
		if dur := r.EndedAt.Sub(r.StartedAt); dur > 0 {
			v.Duration = dur.Round(time.Second).String()
		}
	}
	if r.LastHeartbeatAt != nil {
		v.LastHeartbeat = r.LastHeartbeatAt.Format("15:04:05")
		v.SilentFor = time.Since(*r.LastHeartbeatAt).Round(time.Second).String()
	} else if !r.StartedAt.IsZero() {
		v.SilentFor = time.Since(r.StartedAt).Round(time.Second).String() + " (no heartbeat ever)"
	}
	if v.Owner != "" && v.Repo != "" && v.PRNumber > 0 {
		v.PRURL = "https://github.com/" + v.Owner + "/" + v.Repo + "/pull/" + strconv.Itoa(v.PRNumber)
	}
	return v
}

// RunsRow is the rowsRow used in the runs-list view.
// Replaces the prior `runsRow` that embedded store.Run.
// The Cost / Status / StartedAt / Duration are
// pre-formatted strings; the embed-free shape means
// a future store.Run column does not auto-leak here.
type RunsRow struct {
	ID            string
	Status        string
	StartedAt     string
	Duration      string
	Cost          string
	Owner         string
	Repo          string
	PRNumber      int
	FailureClass  string
}

// newRunsRow is the conversion from the bulk-fetched
// (Run, Telemetry) page to the view row. The store's
// Run is the source of truth; the cost is the only
// telemetry-derived field.
func newRunsRow(r store.Run, t store.Telemetry) RunsRow {
	row := RunsRow{
		ID:            r.ID,
		Status:        string(r.Status),
		Owner:         r.Owner,
		Repo:          r.Repo,
		PRNumber:      r.PRNumber,
		FailureClass:  r.FailureClass,
	}
	if !r.StartedAt.IsZero() {
		row.StartedAt = r.StartedAt.Format("2006-01-02 15:04")
	}
	if r.EndedAt != nil {
		row.Duration = r.EndedAt.Sub(r.StartedAt).Round(time.Second).String()
	}
	if t.CostUSD > 0 {
		row.Cost = "$" + strconv.FormatFloat(t.CostUSD, 'f', 4, 64)
	}
	return row
}

// LiveRow is the rowsRow used in the live view. Same
// rationale as RunsRow: an explicit DTO so the embed
// does not leak future store columns.
type LiveRow struct {
	ID              string
	Owner           string
	Repo            string
	PRNumber        int
	Status          string
	StartedAt       string
	LastHeartbeat   string
	SilentFor       string
	FailureClass    string
}

// newLiveRow is the conversion from store.Run to a
// live-view row. The "SilentFor" computation is
// centralized here (was inline in the old serveLive
// path) so a future "stuck" rule lands in one place.
func newLiveRow(r store.Run) LiveRow {
	row := LiveRow{
		ID:           r.ID,
		Owner:        r.Owner,
		Repo:         r.Repo,
		PRNumber:     r.PRNumber,
		Status:       string(r.Status),
		FailureClass: r.FailureClass,
	}
	if !r.StartedAt.IsZero() {
		row.StartedAt = r.StartedAt.Format("15:04:05")
	}
	if r.LastHeartbeatAt != nil {
		row.LastHeartbeat = r.LastHeartbeatAt.Format("15:04:05")
		row.SilentFor = time.Since(*r.LastHeartbeatAt).Round(time.Second).String()
	} else if !r.StartedAt.IsZero() {
		row.SilentFor = time.Since(r.StartedAt).Round(time.Second).String() + " (no heartbeat ever)"
	}
	return row
}

// ExceptionRow is the row used in the exception dock.
// Same rationale: explicit DTO, no embed.
type ExceptionRow struct {
	ID            string
	Owner         string
	Repo          string
	PRNumber      int
	FailureClass  string
	StartedAt     string
	Error         string
}

// newExceptionRow is the conversion from store.Run to
// the exception view row.
func newExceptionRow(r store.Run) ExceptionRow {
	row := ExceptionRow{
		ID:           r.ID,
		Owner:        r.Owner,
		Repo:         r.Repo,
		PRNumber:     r.PRNumber,
		FailureClass: r.FailureClass,
		Error:        r.Error,
	}
	if !r.StartedAt.IsZero() {
		row.StartedAt = r.StartedAt.Format("2006-01-02 15:04")
	}
	return row
}
