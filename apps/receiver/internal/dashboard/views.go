package dashboard

import (
	"fmt"
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
	ID              string
	Owner           string
	Repo            string
	PRNumber        int
	Status          string
	StartedAt       string
	EndedAt         string
	Duration        string
	FailureClass    string
	Error           string
	PRURL           string
	CommitSHA       string
	ReviewNumber    int
	Reason          string
	LastHeartbeatAt string
	SilentFor       string
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
		ID:           r.ID,
		Owner:        r.Owner,
		Repo:         r.Repo,
		PRNumber:     r.PRNumber,
		Status:       string(r.Status),
		FailureClass: r.FailureClass,
		Error:        r.Error,
		CommitSHA:    r.CommitSHA,
		ReviewNumber: r.ReviewNumber,
		Reason:       r.Reason,
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
		v.LastHeartbeatAt = r.LastHeartbeatAt.Format("15:04:05")
	}
	v.SilentFor = silentFor(r.StartedAt, r.LastHeartbeatAt)
	if v.Owner != "" && v.Repo != "" && v.PRNumber > 0 {
		v.PRURL = "https://github.com/" + v.Owner + "/" + v.Repo + "/pull/" + strconv.Itoa(v.PRNumber)
	}
	return v
}

// silentFor is the single home of the "how long has this
// run been silent" rule, shared by newRunView and
// newLiveRow. A future "stuck" threshold lands here once
// instead of in both converters. A nil heartbeat falls
// back to started-at so a run that never heartbeated
// still reports elapsed silence.
func silentFor(startedAt time.Time, lastHeartbeatAt *time.Time) string {
	if lastHeartbeatAt != nil {
		return time.Since(*lastHeartbeatAt).Round(time.Second).String()
	}
	if !startedAt.IsZero() {
		return time.Since(startedAt).Round(time.Second).String() + " (no heartbeat ever)"
	}
	return ""
}

// TelemetryView is the run-detail DTO for the aggregate
// OpenRouter telemetry. The receiver stores the QUB-105
// shape (see store.Telemetry); the dashboard renders
// every field the runner forwards. Empty/zero values are
// shown as "—" so a partial payload (older runner, network
// truncation) doesn't read as a clean zero-cost run.
type TelemetryView struct {
	Model               string
	Provider            string
	InputTokens         string
	OutputTokens        string
	TotalTokens         string
	ReasoningTokens     string
	CacheReadTokens     string
	CacheWriteTokens    string
	CostUSD             string
	CostPromptUSD       string
	CostCompletionUSD   string
	CostUpstreamUSD     string
	IsByok              bool
	ServerToolCallsExec int64
	ServerToolCallsReq  int64
	RequestID           string
	DurationMS          string
	StepCount           int
	Has                 bool // true when the store returned a non-empty row
}

// newTelemetryView projects store.Telemetry to the view
// shape. `Has` is false when the store has no telemetry
// row for the run (the runner never posted one, or the
// run died before the narrator); the template uses it to
// render an "no telemetry yet" empty state instead of
// zero-everywhere.
func newTelemetryView(t store.Telemetry) TelemetryView {
	if t.Model == "" {
		// No telemetry row at all — the runner never posted
		// the aggregate rollup. Return a view with Has=false
		// so the template renders the empty state.
		return TelemetryView{Has: false}
	}
	return TelemetryView{
		Has: true,
		// The model + provider are non-numeric; render them
		// verbatim (the store filters empty strings out via
		// the JSON omitempty tags upstream).
		Model: t.Model,
		Provider: func() string {
			if t.Provider == "" {
				return "openrouter"
			}
			return t.Provider
		}(),
		// Numeric fields: 0 → "—" so a partial payload
		// doesn't read as "the model used 0 tokens". The
		// raw value is preserved as data-* on the <td>
		// (template) for the "0 means 0" cases (e.g.
		// step_count really is 1 for the single-turn
		// path). The string-form is the only thing the
		// template renders.
		InputTokens:       fmtInt(t.InputTokens),
		OutputTokens:      fmtInt(t.OutputTokens),
		TotalTokens:       fmtInt(t.TotalTokens),
		ReasoningTokens:   fmtInt(t.ReasoningTokens),
		CacheReadTokens:   fmtInt(t.CacheReadTokens),
		CacheWriteTokens:  fmtInt(t.CacheWriteTokens),
		CostUSD:           fmtCost(t.CostUSD),
		CostPromptUSD:     fmtCost(t.CostPromptUSD),
		CostCompletionUSD: fmtCost(t.CostCompletionUSD),
		CostUpstreamUSD:   fmtCost(t.CostUpstreamUSD),
		IsByok:            t.IsByok,
		ServerToolCallsExec: t.ServerToolCallsExec,
		ServerToolCallsReq:  t.ServerToolCallsReq,
		RequestID:           ptrOrEmpty(t.RequestID),
		DurationMS:          ptrIntOrEmpty(t.DurationMS),
		StepCount:           t.StepCount,
	}
}

// LensTelemetryView is the per-lens DTO. Same rationale as
// TelemetryView: every field the dashboard renders passes
// through this converter so a future store.LensTelemetry
// column does not auto-leak to the wire. The "Has" field
// is implicit — a nil entry renders as an empty state.
type LensTelemetryView struct {
	Lens             string
	Model            string
	Provider         string
	InputTokens      string
	OutputTokens     string
	ReasoningTokens  string
	CacheReadTokens  string
	CacheWriteTokens string
	CostUSD          string
	StepCount        int
}

// newLensTelemetryView projects a single store.LensTelemetry
// row to the view shape. The template iterates the slice
// and renders one row per lens.
func newLensTelemetryView(l store.LensTelemetry) LensTelemetryView {
	return LensTelemetryView{
		Lens:             l.Lens,
		Model:            l.Model,
		Provider:         l.Provider,
		InputTokens:      fmtInt(l.InputTokens),
		OutputTokens:     fmtInt(l.OutputTokens),
		ReasoningTokens:  fmtInt(l.ReasoningTokens),
		CacheReadTokens:  fmtInt(l.CacheReadTokens),
		CacheWriteTokens: fmtInt(l.CacheWriteTokens),
		CostUSD:          fmtCost(l.CostUSD),
		StepCount:        l.StepCount,
	}
}

// fmtInt formats a token count for display. 0 → "—" so a
// missing metric reads as "not reported" instead of "0
// tokens", which the operator would otherwise misread as
// "the model used nothing."
func fmtInt(v int64) string {
	if v == 0 {
		return "—"
	}
	return strconv.FormatInt(v, 10)
}

// fmtCost formats a USD cost for display. 0 → "—" (same
// reasoning as fmtInt). Non-zero values get 6 decimal
// places — OpenRouter costs are sub-cent for most reviews
// and the dashboard's run cost column uses 4; the telemetry
// view uses 6 so a $0.000123 expert call is readable.
func fmtCost(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("$%.6f", v)
}

// ptrOrEmpty returns the dereferenced value or "" for a
// nil pointer. Used for nullable TEXT / INTEGER columns
// the runner may have left unset.
func ptrOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ptrIntOrEmpty formats an int64 pointer with the same
// "0 → —" rule as fmtInt. The runner forwards duration_ms
// as *int64; the dashboard renders the value in ms.
func ptrIntOrEmpty(p *int64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%d ms", *p)
}

// RunsRow is the row used in the runs-list view.
// Replaces the prior `runsRow` that embedded store.Run.
// The Cost / Status / StartedAt / Duration are
// pre-formatted strings; the embed-free shape means
// a future store.Run column does not auto-leak here.
type RunsRow struct {
	ID           string
	Status       string
	StartedAt    string
	Duration     string
	Cost         string
	Owner        string
	Repo         string
	PRNumber     int
	FailureClass string
}

// newRunsRow is the conversion from the bulk-fetched
// (Run, Telemetry) page to the view row. The store's
// Run is the source of truth; the cost is the only
// telemetry-derived field.
func newRunsRow(r store.Run, t store.Telemetry) RunsRow {
	row := RunsRow{
		ID:           r.ID,
		Status:       string(r.Status),
		Owner:        r.Owner,
		Repo:         r.Repo,
		PRNumber:     r.PRNumber,
		FailureClass: r.FailureClass,
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

// LiveRow is the row used in the live view. Same
// rationale as RunsRow: an explicit DTO so the embed
// does not leak future store columns.
type LiveRow struct {
	ID              string
	Owner           string
	Repo            string
	PRNumber        int
	Status          string
	StartedAt       string
	LastHeartbeatAt string
	SilentFor       string
	FailureClass    string
}

// newLiveRow is the conversion from store.Run to a
// live-view row. The "SilentFor" computation lives in
// silentFor (shared with newRunView) so a future
// "stuck" rule lands in one place.
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
		row.LastHeartbeatAt = r.LastHeartbeatAt.Format("15:04:05")
	}
	row.SilentFor = silentFor(r.StartedAt, r.LastHeartbeatAt)
	return row
}

// ExceptionRow is the row used in the exception dock.
// Same rationale: explicit DTO, no embed.
type ExceptionRow struct {
	ID           string
	Owner        string
	Repo         string
	PRNumber     int
	FailureClass string
	StartedAt    string
	Error        string
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
