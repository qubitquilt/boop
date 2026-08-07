package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RunStatus is the lifecycle state of a single review. The values
// are the same as the K8s Job's humanStatus ("Running", "Complete",
// "Failed") plus "pending" for the moment between webhook accept
// and Job start, and "queued" for a re-delivery that was deduped
// before the runner took over. The strings are stable: the
// dashboard reads them directly.
type RunStatus string

const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
)

// Run is one review. The id is the K8s Job name (e.g.
// "boop-qubitquilt-boop-42-a1b2c3d") so the dashboard can join the
// run row to the live Job for the duration the Job is in the
// namespace. After the Job is GC'd, the id is still meaningful —
// it carries the owner, repo, PR number, and short SHA.
//
// FailureClass and LastHeartbeatAt are QUB-108/QUB-109
// additions: FailureClass is set by the receiver at
// UpsertRun time from the K8s container exit reason (or by
// the runner at end-of-run for non-pod failures like
// json_parse_fail); LastHeartbeatAt is updated by the
// receiver on every runner heartbeat POST and powers the
// stuck-runs panel.
//
// ParentRunID and SupersededByID are QUB-110 additions:
// ParentRunID points to the run this one was re-run from;
// SupersededByID points to the run that re-ran this one.
// The two columns are not symmetric — a re-run never
// branches, so SupersededByID is at most one row. The
// dashboard's "vertical timeline" view walks the chain
// via ParentRunID.
type Run struct {
	ID              string
	Owner           string
	Repo            string
	PRNumber        int
	CommitSHA       string
	BaseRef         string
	ReviewNumber    int
	Reason          string
	InstallationID  int64
	Status          RunStatus
	StartedAt       time.Time
	EndedAt         *time.Time
	DurationMS      *int64
	Error           string
	FailureClass    string
	LastHeartbeatAt *time.Time
	ParentRunID     string
	SupersededByID  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UpsertRun inserts the run if it does not exist, or updates the
// mutable fields if it does. The PRIMARY KEY is the Job name; a
// re-delivery for the same head SHA hits the same row.
//
// Mutable fields (status, ended_at, duration_ms, error, updated_at)
// are always overwritten. Immutable fields (owner, repo, pr_number,
// commit_sha, base_ref, review_number, reason, installation_id,
// started_at) are left alone on UPDATE — they were captured at
// webhook time and should not drift if a re-delivery races the
// runner's first status update.
//
// Returns the final row (post-merge) so callers can echo it back
// without a second SELECT.
func (s *Store) UpsertRun(ctx context.Context, r Run) (Run, error) {
	if r.ID == "" {
		return Run{}, errors.New("store: UpsertRun: empty id")
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (
			id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class,
			parent_run_id, superseded_by_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status           = excluded.status,
			ended_at         = excluded.ended_at,
			duration_ms      = excluded.duration_ms,
			error            = excluded.error,
			failure_class    = excluded.failure_class,
			parent_run_id    = excluded.parent_run_id,
			superseded_by_id = excluded.superseded_by_id,
			updated_at       = excluded.updated_at
	`,
		r.ID, r.Owner, r.Repo, r.PRNumber, r.CommitSHA, r.BaseRef,
		r.ReviewNumber, nullString(r.Reason), nullInt64(r.InstallationID), string(r.Status),
		r.StartedAt.UTC().Format(time.RFC3339Nano), nullTimePtr(r.EndedAt), nullInt64Ptr(r.DurationMS), nullString(r.Error),
		nullString(r.FailureClass),
		nullString(r.ParentRunID), nullString(r.SupersededByID),
		r.CreatedAt.UTC().Format(time.RFC3339Nano), r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return Run{}, fmt.Errorf("store: upsert run: %w", err)
	}
	return s.GetRun(ctx, r.ID)
}

// UpdateRunStatus is the narrow entry point the runner uses to
// advance a run's status as it works through its stages. It writes
// status + (optionally) ended_at/duration_ms/error in one shot so
// the dashboard sees a single coherent transition, not two writes
// with an observable intermediate state.
//
// The status is the only required field; ended/duration/error are
// only updated when the caller supplies them. Returns the updated
// row, or sql.ErrNoRows if the run does not exist (the runner
// started before the receiver committed the row, which is fine —
// the runner will retry on the next stage).
func (s *Store) UpdateRunStatus(ctx context.Context, id string, status RunStatus, endedAt *time.Time, durationMS *int64, errMsg string) (Run, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET
			status      = ?,
			ended_at    = COALESCE(?, ended_at),
			duration_ms = COALESCE(?, duration_ms),
			error       = COALESCE(NULLIF(?, ''), error),
			updated_at  = ?
		WHERE id = ?
	`,
		string(status),
		nullTimePtr(endedAt),
		nullInt64Ptr(durationMS),
		errMsg,
		now.UTC().Format(time.RFC3339Nano),
		id,
	)
	if err != nil {
		return Run{}, fmt.Errorf("store: update run status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, fmt.Errorf("store: update run status rows: %w", err)
	}
	if n == 0 {
		return Run{}, sql.ErrNoRows
	}
	return s.GetRun(ctx, id)
}

// SetRunFailureClass writes just the failure_class column on a
// run row. Used by the K8s reconciler (Phase 1's post-createJob
// path) to backfill the exit reason once the Job terminates.
// Splitting this from UpdateRunStatus keeps the
// already-witnessed status/ended_at fields from being
// overwritten by a later reconciler tick, and it lets the
// reconciler do an empty-string clear ("no failure class
// known") without coupling that to a status update.
//
// Returns sql.ErrNoRows if the run has been pruned between
// the reconciler's read of the Job and the write here — the
// reconciler treats that as a no-op so it doesn't have to
// retry on a retention race.
func (s *Store) SetRunFailureClass(ctx context.Context, id, failureClass string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET failure_class = ?, updated_at = ? WHERE id = ?
	`, nullString(failureClass), now, id)
	if err != nil {
		return fmt.Errorf("store: set run failure class: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set run failure class rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetSupersededBy writes just the superseded_by_id column
// on a run row. Used by the re-run flow (Phase 3) to
// backfill the lineage pointer without re-touching any
// other field. The prior run's status, ended_at, error,
// failure_class, etc. stay exactly as the runner left
// them; only the new lineage pointer lands.
//
// Returns sql.ErrNoRows if the prior has been pruned —
// the re-run flow treats that as a non-fatal no-op so
// the new run row still lands and the operator's
// "show me the lineage" view is consistent (the prior
// just renders as "(pruned)" on the dashboard side).
func (s *Store) SetSupersededBy(ctx context.Context, priorID, newID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET superseded_by_id = ?, updated_at = ? WHERE id = ?
	`, nullString(newID), now, priorID)
	if err != nil {
		return fmt.Errorf("store: set superseded by: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set superseded by rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetRun returns the run with the given id, or ErrUnknownRun
// (wrapping sql.ErrNoRows) if none exists. Used by the
// runner's POST endpoints to look up the row before they
// update it.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class, last_heartbeat_at,
			COALESCE(parent_run_id, ''), COALESCE(superseded_by_id, ''),
			created_at, updated_at
		FROM runs WHERE id = ?
	`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrUnknownRun
	}
	return r, err
}

// ListRunsFilter narrows a ListRuns call. Empty fields are ignored
// (no constraint). From/To are inclusive bounds on started_at; if
// From is zero, no lower bound; if To is zero, no upper bound. The
// cursor is the (started_at, id) of the last item in the previous
// page, or empty for the first page. Limit is clamped to 1..200.
type ListRunsFilter struct {
	Owner          string
	Repo           string
	Status         RunStatus
	FailureClass   string
	From           time.Time
	To             time.Time
	InstallationID int64
	Cursor         string
	Limit          int
}

// ListRunsResult is the page returned by ListRuns.
type ListRunsResult struct {
	Runs       []Run
	NextCursor string
}

// ListRuns returns runs ordered newest-first by (started_at, id).
// Pagination is keyset-style on (started_at DESC, id DESC): the
// cursor encodes the boundary, and the SQL adds a `started_at < ?`
// (or `started_at = ? AND id < ?`) WHERE clause. Keyset is the
// right choice here because the run table is append-mostly and the
// "rows per page" shape matches the dashboard's infinite-scroll.
//
// The dashboard never paginates more than 200 rows at a time;
// arbitrary deep history is the stats endpoint's job.
func (s *Store) ListRuns(ctx context.Context, f ListRunsFilter) (ListRunsResult, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var (
		conds []string
		args  []any
	)
	if f.Owner != "" {
		conds = append(conds, "owner = ?")
		args = append(args, f.Owner)
	}
	if f.Repo != "" {
		conds = append(conds, "repo = ?")
		args = append(args, f.Repo)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.FailureClass != "" {
		conds = append(conds, "failure_class = ?")
		args = append(args, f.FailureClass)
	}
	if f.InstallationID != 0 {
		conds = append(conds, "installation_id = ?")
		args = append(args, f.InstallationID)
	}
	if !f.From.IsZero() {
		conds = append(conds, "started_at >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339Nano))
	}
	if !f.To.IsZero() {
		conds = append(conds, "started_at <= ?")
		args = append(args, f.To.UTC().Format(time.RFC3339Nano))
	}
	if f.Cursor != "" {
		cs, id, ok := decodeCursor(f.Cursor)
		if !ok {
			return ListRunsResult{}, fmt.Errorf("store: malformed cursor")
		}
		// Format the cursor's time to the same RFC3339Nano string
		// the rows were written with; otherwise the driver would
		// pick its own default format and the comparison would
		// miss every row.
		csStr := cs.UTC().Format(time.RFC3339Nano)
		conds = append(conds, "(started_at < ? OR (started_at = ? AND id < ?))")
		args = append(args, csStr, csStr, id)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// limit+1 lets us tell whether another page exists without a
	// second COUNT query.
	q := fmt.Sprintf(`
		SELECT id, owner, repo, pr_number, commit_sha, base_ref,
			review_number, reason, installation_id, status,
			started_at, ended_at, duration_ms, error,
			failure_class, last_heartbeat_at,
			COALESCE(parent_run_id, ''), COALESCE(superseded_by_id, ''),
			created_at, updated_at
		FROM runs
		%s
		ORDER BY started_at DESC, id DESC
		LIMIT ?
	`, where)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ListRunsResult{}, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return ListRunsResult{}, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return ListRunsResult{}, fmt.Errorf("store: rows: %w", err)
	}

	next := ""
	if len(out) > limit {
		last := out[limit-1]
		next = encodeCursor(last.StartedAt, last.ID)
		out = out[:limit]
	}
	return ListRunsResult{Runs: out, NextCursor: next}, nil
}

// encodeCursor / decodeCursor are the boundary format for keyset
// pagination. The cursor is "<rfc3339nano>|<id>" — a single pipe
// so a malformed cursor is easy to detect. We use pipe instead of
// a JSON envelope because the only thing that ever decodes a
// cursor is this package; the wire shape is an implementation
// detail.
func encodeCursor(t time.Time, id string) string {
	return t.UTC().Format(time.RFC3339Nano) + "|" + id
}

func decodeCursor(c string) (time.Time, string, bool) {
	idx := strings.IndexByte(c, '|')
	if idx <= 0 || idx == len(c)-1 {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, c[:idx])
	if err != nil {
		return time.Time{}, "", false
	}
	return t, c[idx+1:], true
}

// rowScanner is the common shape for sql.Row and sql.Rows so the
// same scan function works for both.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(r rowScanner) (Run, error) {
	var (
		rr           Run
		status       string
		startedStr   string
		endedPtr     sql.NullString
		durPtr       sql.NullInt64
		errPtr       sql.NullString
		baseRef      sql.NullString
		reason       sql.NullString
		installID    sql.NullInt64
		failureClass sql.NullString
		heartbeatPtr sql.NullString
		parentID     string
		supersededBy string
		createdStr   string
		updatedStr   string
	)
	if err := r.Scan(
		&rr.ID, &rr.Owner, &rr.Repo, &rr.PRNumber, &rr.CommitSHA, &baseRef,
		&rr.ReviewNumber, &reason, &installID, &status,
		&startedStr, &endedPtr, &durPtr, &errPtr,
		&failureClass, &heartbeatPtr,
		&parentID, &supersededBy,
		&createdStr, &updatedStr,
	); err != nil {
		return Run{}, err
	}
	rr.Status = RunStatus(status)
	rr.BaseRef = baseRef.String
	rr.Reason = reason.String
	rr.FailureClass = failureClass.String
	rr.ParentRunID = parentID
	rr.SupersededByID = supersededBy
	if installID.Valid {
		rr.InstallationID = installID.Int64
	}
	if t, err := time.Parse(time.RFC3339Nano, startedStr); err == nil {
		rr.StartedAt = t
	}
	if endedPtr.Valid {
		if t, err := time.Parse(time.RFC3339Nano, endedPtr.String); err == nil {
			rr.EndedAt = &t
		}
	}
	if durPtr.Valid {
		v := durPtr.Int64
		rr.DurationMS = &v
	}
	rr.Error = errPtr.String
	if heartbeatPtr.Valid {
		if t, err := time.Parse(time.RFC3339Nano, heartbeatPtr.String); err == nil {
			rr.LastHeartbeatAt = &t
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, createdStr); err == nil {
		rr.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, updatedStr); err == nil {
		rr.UpdatedAt = t
	}
	return rr, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullTimePtr(p *time.Time) any {
	if p == nil {
		return nil
	}
	return p.UTC().Format(time.RFC3339Nano)
}
