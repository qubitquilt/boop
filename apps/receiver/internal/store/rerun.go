package store

// Re-run lineage (QUB-110).
//
// A re-run is a new Job that targets the same head SHA, the
// same PR, but a different attempt number. The new Job
// inherits the prior run's posted comments and is told
// (through BOOP_PARENT_RUN_ID) not to duplicate them. The
// lineage chain — parent_run_id / superseded_by_id on
// the runs table — is what makes the dashboard's "show
// me the chain" view possible.
//
// The Job name convention is:
//
//	boop-{owner}-{repo}-{pr}-{sha7}      original
//	boop-{owner}-{repo}-{pr}-{sha7}-r1   first re-run
//	boop-{owner}-{repo}-{pr}-{sha7}-r2   second re-run
//
// n = 1 + count of existing "-r<n>" jobs for that (owner,
// repo, pr, sha7). The original's id has no "-r" suffix, so
// `r1` is the first re-run, `r2` the second, etc. Bypassing
// claimJobSlot is what allows multiple concurrent re-runs
// to coexist (the original webhook's dedup is by head SHA,
// but a re-run is operator-initiated and may legitimately
// run while the original is still active).

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// rerunSuffix is the regex that recognizes a re-run Job
// name. The capture group is the integer attempt number
// ("", "1", "2", ...). Used by NextRerunJobName to count
// existing re-runs for a (owner, repo, pr, sha7) tuple.
var rerunSuffix = regexp.MustCompile(`-r(\d+)$`)

// CountRerunJobsForSHA returns the number of existing
// re-run Jobs for the (owner, repo, pr, sha7) tuple. The
// new re-run's attempt number is this count + 1, since the
// first re-run is "-r1" (count=0 → 1).
func (s *Store) CountRerunJobsForSHA(ctx context.Context, owner, repo string, pr int, sha7 string) (int, error) {
	prefix := buildJobNamePrefix(owner, repo, pr, sha7) + "-r"
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM runs WHERE id LIKE ? || '%' AND id LIKE '%-r%'
	`, prefix)
	if err != nil {
		return 0, fmt.Errorf("store: count reruns: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("store: scan rerun id: %w", err)
		}
		// Defensive: the LIKE filter is approximate.
		// Re-verify the suffix matches the expected
		// pattern so a future column / row that
		// accidentally has "r" in it doesn't get
		// double-counted.
		if rerunSuffix.MatchString(id) {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: rerun rows: %w", err)
	}
	return count, nil
}

// buildJobNamePrefix is the part of the Job name that is
// shared between the original and every re-run. Used by
// CountRerunJobsForSHA and the handler's rerun Job
// construction.
func buildJobNamePrefix(owner, repo string, pr int, sha7 string) string {
	sanitized := jobNameSanitizerRerun.ReplaceAllString(strings.ToLower(
		fmt.Sprintf("boop-%s-%s-%d-%s", owner, repo, pr, sha7),
	), "-")
	return sanitized
}

// jobNameSanitizerRerun mirrors handler.go's
// jobNameSanitizer. The handler's version is unexported
// and the rerun code lives in a different package, so the
// regex is duplicated here.
var jobNameSanitizerRerun = regexp.MustCompile(`[^a-z0-9-]`)

// Lineage is the result of walking the parent_run_id
// chain from a run. WalkUp is the chain from this run to
// the root (this run first, then parent, then
// grandparent, etc.). WalkDown is the children that were
// re-run from this run (at most one — a re-run never
// branches). The dashboard's run-detail page renders
// WalkUp as a vertical timeline and WalkDown as a
// "superseded by" pill on each row.
type Lineage struct {
	WalkUp   []Run
	WalkDown []Run
}

// WalkLineage walks the parent_run_id chain from start.
// Up to maxDepth levels are returned; the limit is
// defensive against a future bug that creates a cycle
// (the foreign keys don't prevent it because the columns
// are SET NULL on delete, not NO ACTION).
func (s *Store) WalkLineage(ctx context.Context, start string, maxDepth int) (Lineage, error) {
	if maxDepth <= 0 {
		maxDepth = 32
	}
	out := Lineage{}
	// WalkUp
	current := start
	for i := 0; i < maxDepth; i++ {
		row, err := s.GetRun(ctx, current)
		if err != nil {
			break
		}
		out.WalkUp = append(out.WalkUp, row)
		if row.ParentRunID == "" || row.ParentRunID == current {
			break
		}
		current = row.ParentRunID
	}
	// WalkDown: at most one re-run per run, so this is
	// a single SELECT by id. If a re-run never branches
	// (the spec rule), then WalkDown is always len 0 or
	// 1.
	if last := currentRunOrEmpty(out.WalkUp); last != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id FROM runs WHERE parent_run_id = ?
		`, last)
		if err != nil {
			return out, fmt.Errorf("store: walk down: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return out, fmt.Errorf("store: scan down: %w", err)
			}
			child, err := s.GetRun(ctx, id)
			if err == nil {
				out.WalkDown = append(out.WalkDown, child)
			}
		}
		if err := rows.Err(); err != nil {
			return out, fmt.Errorf("store: down rows: %w", err)
		}
	}
	return out, nil
}

func currentRunOrEmpty(rs []Run) string {
	if len(rs) == 0 {
		return ""
	}
	return rs[0].ID
}
