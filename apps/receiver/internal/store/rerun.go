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
//
// RD-002: the Job-name convention used to live in three
// packages — webhook/handler.go's buildJobName, webhook/
// rerun.go's buildJobNameRerun, and this file's
// buildJobNamePrefix — each with its own copy of the
// `[^a-z0-9-]` sanitizer regex. Three copies is three
// places a future contributor could change one and miss
// the others. The canonical helpers now live here as
// BuildJobName / BuildRerunJobName / ShortSHA; the
// webhook package calls them.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// jobNameSanitizer matches any character that is not a
// lower-case letter, digit, or hyphen. The Job name lives
// in a K8s namespace where DNS-1123 rules require this
// subset; an un-sanitized value (e.g. owner="Org/Name")
// would fail the Job-create API call. The single regex
// here is the load-bearing source of truth for the
// receiver.
var jobNameSanitizer = regexp.MustCompile(`[^a-z0-9-]`)

// ShortSHA returns the first 7 characters of sha, or sha
// itself when shorter. Job names embed the 7-char prefix
// because K8s labels are limited to 63 chars and the full
// SHA pushes the Job name over the limit on some names.
func ShortSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

// BuildJobName constructs the Job name for an (owner,
// repo, pr, sha) tuple. Sanitises the input so an owner
// or repo with mixed case / non-ASCII characters does not
// break the K8s name rules. The format is the receiver's
// canonical contract — every Job the webhook creates and
// every re-run the dashboard creates follows the same
// shape.
func BuildJobName(owner, repo string, pr int, sha string) string {
	raw := fmt.Sprintf("boop-%s-%s-%d-%s", owner, repo, pr, ShortSHA(sha))
	return jobNameSanitizer.ReplaceAllString(strings.ToLower(raw), "-")
}

// BuildRerunJobName constructs the Job name for a re-run,
// layering the -r{N} suffix on top of BuildJobName. n=1
// for the first re-run.
func BuildRerunJobName(owner, repo string, pr int, sha string, n int) string {
	if n < 1 {
		n = 1
	}
	return fmt.Sprintf("%s-r%d", BuildJobName(owner, repo, pr, sha), n)
}

// rerunSuffix is the regex that recognizes a re-run Job
// name. The capture group is the integer attempt number
// ("", "1", "2", ...). Used by CountRerunJobsForSHA to
// count existing re-runs for a (owner, repo, pr, sha7)
// tuple.
var rerunSuffix = regexp.MustCompile(`-r(\d+)$`)

// CountRerunJobsForSHA returns the number of existing
// re-run Jobs for the (owner, repo, pr, sha7) tuple. The
// new re-run's attempt number is this count + 1, since the
// first re-run is "-r1" (count=0 → 1).
func (s *Store) CountRerunJobsForSHA(ctx context.Context, owner, repo string, pr int, sha7 string) (int, error) {
	prefix := BuildJobName(owner, repo, pr, sha7) + "-r"
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
