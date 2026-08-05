package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DeepCheck runs PRAGMA quick_check on the database. quick_check
// (not full integrity_check) is the right tool for the /health
// deep probe: it walks the b-tree and verifies page headers
// without the multi-minute scan that integrity_check does. A
// quick_check failure is a strong signal that the file is
// corrupt; the operator should restore from the most recent
// /backups snapshot.
//
// Returns (ok, result, err):
//   - ok is true iff the first row of the result is exactly
//     "ok" (case-insensitive). quick_check is documented to
//     return "ok" on success and any other string on failure;
//     we treat anything else as a fail.
//   - result is the first row of the quick_check output
//     (quick_check can return multiple rows for a structured
//     failure, but the first one is enough to surface in a
//     /health?deep=1 JSON body).
//   - err is non-nil only when the PRAGMA call itself failed
//     (e.g. database is unreachable); a non-"ok" result is
//     not an error.
//
// The deepHealth handler turns a non-ok result into a 503
// response with the result string in the body. The store does
// not log the failure itself — the handler does — so the
// failure is visible to the operator's HTTP-side alerting
// without also spamming the receiver log.
func (s *Store) DeepCheck() (bool, string, error) {
	if s == nil || s.db == nil {
		return false, "", fmt.Errorf("store: nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return false, "", fmt.Errorf("store: quick_check: %w", err)
	}
	defer rows.Close()
	var result string
	if rows.Next() {
		if err := rows.Scan(&result); err != nil {
			return false, "", fmt.Errorf("store: quick_check scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return false, "", fmt.Errorf("store: quick_check rows: %w", err)
	}
	ok := strings.EqualFold(strings.TrimSpace(result), "ok")
	return ok, result, nil
}

// pingWithTimeout is a thin wrapper around db.PingContext that
// bounds the wait. Used by health probes that should not
// block forever on a wedged connection.
func (s *Store) pingWithTimeout(d time.Duration) error {
	if s == nil || s.db == nil {
		return sql.ErrConnDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return s.db.PingContext(ctx)
}
