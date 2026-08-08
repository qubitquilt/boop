// Package render formats API responses for either human (table) or
// machine (--json) consumption. The human tables are hand-laid-out so
// the columns are stable under `column`-style alignment without
// requiring a tabular library dependency.
//
// Every command calls Render(resp, asJSON) at the end. When asJSON is
// false, Render prints a human-friendly table and returns nil; when
// true, it marshals resp to indented JSON and returns it so the
// command can pipe it to stdout verbatim.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/michaelruelas/boop-cli/internal/api"
)

// Render dispatches on the concrete type of v and writes either a
// human table or indented JSON to w. asJSON forces JSON output.
func Render(w io.Writer, v any, asJSON bool) error {
	if asJSON {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	return renderHuman(w, v)
}

// renderHuman picks the table formatter based on the concrete type. It
// returns an error only for unknown types so a missed case fails loud
// during development rather than printing nothing.
func renderHuman(w io.Writer, v any) error {
	switch tv := v.(type) {
	case *api.Health:
		return renderHealth(w, tv)
	case *api.ReviewsResponse:
		return renderReviews(w, tv)
	case *api.InstallationsResponse:
		return renderInstallations(w, tv)
	case *api.ListRunsResponse:
		return renderRuns(w, tv)
	case *api.RerunPreviewResponse:
		return renderRerunPreview(w, tv)
	case *api.RerunResponse:
		return renderRerun(w, tv)
	case *api.StatsResponse:
		return renderStats(w, tv)
	case *api.RunWithTelemetry:
		return renderRunWithTelemetry(w, tv)
	default:
		return fmt.Errorf("render: unsupported type %T", v)
	}
}

func renderHealth(w io.Writer, h *api.Health) error {
	_, err := fmt.Fprintln(w, "boop receiver:", h.Status)
	return err
}

func renderReviews(w io.Writer, r *api.ReviewsResponse) error {
	tab := newTable("NAME", "STATE", "OWNER/REPO", "PR", "COMMIT", "STARTED", "DURATION")
	for _, s := range []struct {
		label string
		items []api.Review
	}{
		{"active", r.Active}, {"recent", r.Recent}, {"failed", r.Failed},
	} {
		if len(s.items) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d)\n", strings.ToUpper(s.label), len(s.items))
		for _, rev := range s.items {
			repo := ""
			if rev.Owner != "" && rev.Repo != "" {
				repo = rev.Owner + "/" + rev.Repo
			}
			started := shortTime(rev.StartTime)
			_, err := tab.row(rev.Name, rev.Status, repo, spr(rev.PR), shortSHA(rev.Commit), started, rev.Duration)
			if err != nil {
				return err
			}
		}
	}
	return tab.flush(w)
}

func renderInstallations(w io.Writer, r *api.InstallationsResponse) error {
	tab := newTable("ID", "ACCOUNT", "TYPE", "REPOS", "PAUSED", "INSTALLED", "FETCHED")
	for _, ins := range r.Installations {
		repoSel := ins.RepositorySelection
		if repoSel == "" {
			repoSel = "all"
		}
		_, err := tab.row(
			spr(ins.ID), ins.AccountLogin, ins.AccountType, repoSel,
			boolStr(ins.Paused), tFormatter(ins.InstalledAt), tFormatter(ins.FetchedAt),
		)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "\ninstalled on %d account(s)\n", len(r.Installations))
	return tab.flush(w)
}

func renderRuns(w io.Writer, r *api.ListRunsResponse) error {
	tab := newTable("RUN ID", "STATUS", "OWNER/REPO", "PR", "COMMIT", "STARTED", "DURATION", "COST")
	for _, rw := range r.Runs {
		repo := rw.Run.Owner + "/" + rw.Run.Repo
		var costStr string
		if rw.Telemetry.Model != "" {
			costStr = fmt.Sprintf("$%0.2f (%s)", rw.Telemetry.CostUSD, rw.Telemetry.Model)
		}
		var durStr string
		if rw.Run.DurationMS > 0 {
			durStr = (time.Duration(rw.Run.DurationMS) * time.Millisecond).Round(time.Second).String()
		}
		_, err := tab.row(
			rw.Run.ID, string(rw.Run.Status), repo,
			spr(rw.Run.PRNumber), shortSHA(rw.Run.CommitSHA),
			tFormatter(rw.Run.StartedAt), durStr, costStr,
		)
		if err != nil {
			return err
		}
	}
	if r.NextCursor != "" {
		fmt.Fprintf(w, "\nmore: pass --cursor %q for the next page\n", r.NextCursor)
	}
	return tab.flush(w)
}

func renderRunWithTelemetry(w io.Writer, rw *api.RunWithTelemetry) error {
	_, err := fmt.Fprintf(w, "run: %s\n", rw.Run.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  status: %s\n", rw.Run.Status)
	fmt.Fprintf(w, "  repo:   %s/%s  pr #%d\n", rw.Run.Owner, rw.Run.Repo, rw.Run.PRNumber)
	fmt.Fprintf(w, "  sha:    %s\n", shortSHA(rw.Run.CommitSHA))
	if rw.Run.Reason != "" {
		fmt.Fprintf(w, "  reason: %s\n", rw.Run.Reason)
	}
	if rw.Run.FailureClass != "" {
		fmt.Fprintf(w, "  fault:  %s\n", rw.Run.FailureClass)
	}
	if rw.Run.Error != "" {
		fmt.Fprintf(w, "  error:  %s\n", rw.Run.Error)
	}
	if !rw.Run.StartedAt.IsZero() {
		fmt.Fprintf(w, "  started: %s\n", rw.Run.StartedAt.UTC().Format(time.RFC3339))
	}
	if rw.Run.DurationMS > 0 {
		fmt.Fprintf(w, "  duration: %s\n", (time.Duration(rw.Run.DurationMS)*time.Millisecond).Round(time.Second))
	}
	if rw.Telemetry.Model != "" {
		fmt.Fprintf(w, "  model:  %s ($%0.4f)\n", rw.Telemetry.Model, rw.Telemetry.CostUSD)
		fmt.Fprintf(w, "  tokens: in=%d out=%d reasoning=%d cache_r=%d cache_w=%d steps=%d\n",
			rw.Telemetry.InputTokens, rw.Telemetry.OutputTokens, rw.Telemetry.ReasoningTokens,
			rw.Telemetry.CacheReadTokens, rw.Telemetry.CacheWriteTokens, rw.Telemetry.StepCount)
	} else {
		fmt.Fprintln(w, "  model:  (no telemetry recorded)")
	}
	if rw.Run.ParentRunID != "" {
		fmt.Fprintf(w, "  parent: %s\n", rw.Run.ParentRunID)
	}
	if rw.Run.SupersededByID != "" {
		fmt.Fprintf(w, "  superseded by: %s\n", rw.Run.SupersededByID)
	}
	return nil
}

func renderRerunPreview(w io.Writer, r *api.RerunPreviewResponse) error {
	fmt.Fprintln(w, "prior run:")
	renderRerunRun(w, &r.Prior, "  ")
	fmt.Fprintln(w, "\nnew run (preview):")
	renderRerunRun(w, &r.New, "  ")
	return nil
}

func renderRerunRun(w io.Writer, r *api.RerunPreviewRun, indent string) {
	fmt.Fprintf(w, "%srun_id:  %s\n", indent, r.RunID)
	fmt.Fprintf(w, "%sjob:     %s\n", indent, r.JobName)
	fmt.Fprintf(w, "%sstatus:  %s\n", indent, r.Status)
	if r.Model != "" {
		fmt.Fprintf(w, "%scost was: %s\n", indent, r.Model)
	}
	fmt.Fprintf(w, "%ssha:     %s\n", indent, shortSHA(r.HeadSHA))
	if r.StartedAt != "" {
		fmt.Fprintf(w, "%sstarted: %s\n", indent, r.StartedAt)
	}
	if r.Duration > 0 {
		fmt.Fprintf(w, "%sduration: %s\n", indent, (time.Duration(r.Duration)*time.Millisecond).Round(time.Second))
	}
}

func renderRerun(w io.Writer, r *api.RerunResponse) error {
	fmt.Fprintf(w, "re-run created\n")
	fmt.Fprintf(w, "  prior run: %s\n", r.PriorRunID)
	fmt.Fprintf(w, "  new run:   %s\n", r.NewRunID)
	return nil
}

func renderStats(w io.Writer, s *api.StatsResponse) error {
	fmt.Fprintf(w, "window: %s → %s (bucket: %s)\n\n",
		s.From.UTC().Format(time.RFC3339), s.To.UTC().Format(time.RFC3339), s.Bucket)
	fmt.Fprintln(w, "summary:")
	fmt.Fprintf(w, "  total runs:    %d\n", s.Summary.TotalRuns)
	fmt.Fprintf(w, "  succeeded:     %d\n", s.Summary.SucceededRuns)
	fmt.Fprintf(w, "  failed:        %d\n", s.Summary.FailedRuns)
	fmt.Fprintf(w, "  running:       %d\n", s.Summary.RunningRuns)
	fmt.Fprintf(w, "  success rate:  %.1f%%\n", s.Summary.SuccessRate*100)
	fmt.Fprintf(w, "  total cost:    $%0.2f\n", s.Summary.TotalCostUSD)
	fmt.Fprintf(w, "  total tokens:  %s\n", comma(s.Summary.TotalTokens))
	fmt.Fprintf(w, "  avg duration:  %s\n", durStr(s.Summary.AvgDurationMS))
	fmt.Fprintf(w, "  p50 / p95:     %s / %s\n", durStr(s.Summary.P50DurationMS), durStr(s.Summary.P95DurationMS))
	fmt.Fprintf(w, "  unique repos:  %d\n", s.Summary.UniqueRepos)
	fmt.Fprintf(w, "  unique installs: %d\n", s.Summary.UniqueInstalls)

	if len(s.Buckets) > 0 {
		fmt.Fprintln(w, "\ntime series:")
		tab := newTable("BUCKET", "RUNS", "OK", "FAIL", "COST", "TOKENS")
		for _, b := range s.Buckets {
			_, err := tab.row(
				b.BucketStart.UTC().Format("2006-01-02 15:04"),
				spr(b.Runs), spr(b.Succeeded), spr(b.Failed),
				fmt.Sprintf("$%0.2f", b.CostUSD), comma(b.InputTokens+b.OutputTokens),
			)
			if err != nil {
				return err
			}
		}
		if err := tab.flush(w); err != nil {
			return err
		}
	}

	if len(s.ByRepo) > 0 {
		fmt.Fprintln(w, "\nby repo (top):")
		tab := newTable("REPO", "RUNS", "OK", "FAIL", "RATE", "COST")
		for _, r := range s.ByRepo {
			_, err := tab.row(
				r.Owner+"/"+r.Repo,
				spr(r.Runs), spr(r.Succeeded), spr(r.Failed),
				fmt.Sprintf("%.1f%%", r.SuccessRate*100),
				fmt.Sprintf("$%0.2f", r.TotalCostUSD),
			)
			if err != nil {
				return err
			}
		}
		if err := tab.flush(w); err != nil {
			return err
		}
	}

	if len(s.ByModel) > 0 {
		fmt.Fprintln(w, "\nby model:")
		tab := newTable("MODEL", "RUNS", "COST", "IN", "OUT")
		for _, m := range s.ByModel {
			_, err := tab.row(
				m.Model, spr(m.Runs), fmt.Sprintf("$%0.2f", m.TotalCostUSD),
				comma(m.InputTokens), comma(m.OutputTokens),
			)
			if err != nil {
				return err
			}
		}
		if err := tab.flush(w); err != nil {
			return err
		}
	}
	return nil
}

// Helpers

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func shortSHA(s string) string {
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}

func shortTime(s string) string {
	if len(s) < 16 {
		return s
	}
	return s[:16]
}

func tFormatter(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func durStr(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

func comma(n int64) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	var out []string
	for i := 0; i < len(s); i += 3 {
		if i > 0 {
			out = append([]string{","}, out...)
		}
		// Not the most efficient, but only used for display of
		// token counts which fit in int64.
		_ = i
	}
	// Use a simple reverse-chunked formatter instead of the
	// broken loop above.
	return formatComma(s)
}

func formatComma(s string) string {
	var b strings.Builder
	n := len(s)
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func spr(i int) string {
	if i == 0 {
		return "-"
	}
	return fmt.Sprint(i)
}

// minTable is a tiny fixed-column-width table writer. Each row is
// stored as a slice of strings; flush computes the column widths in
// one pass and writes aligned rows. No external dependency.
type minTable struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *minTable {
	return &minTable{headers: headers}
}

func (t *minTable) row(cells ...string) (*minTable, error) {
	if len(cells) != len(t.headers) {
		return t, fmt.Errorf("table: row has %d cells, header has %d", len(cells), len(t.headers))
	}
	t.rows = append(t.rows, cells)
	return t, nil
}

func (t *minTable) flush(w io.Writer) error {
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		widths[i] = len(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	// Header
	line := make([]string, len(t.headers))
	for i, h := range t.headers {
		line[i] = pad(h, widths[i])
	}
	if _, err := fmt.Fprintln(w, strings.Join(line, "  ")); err != nil {
		return err
	}
	// Separator
	sep := make([]string, len(t.headers))
	for i, w := range widths {
		sep[i] = strings.Repeat("-", w)
	}
	if _, err := fmt.Fprintln(w, strings.Join(sep, "  ")); err != nil {
		return err
	}
	// Rows
	for _, r := range t.rows {
		line := make([]string, len(r))
		for i, c := range r {
			line[i] = pad(c, widths[i])
		}
		if _, err := fmt.Fprintln(w, strings.Join(line, "  ")); err != nil {
			return err
		}
	}
	return nil
}

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}
