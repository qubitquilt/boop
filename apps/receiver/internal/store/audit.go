package store

// Audit log (QUB-112).
//
// Every dashboard-initiated action appends a row to
// audit_events. The load-bearing column is `actor` —
// without it, an admin action is unattributable, and a
// future compliance audit cannot answer "who paused
// this install?" / "who zeroed this cost?" / "who
// re-ran this run?".
//
// The action vocabulary is small and free-form. A
// future schema bump can normalize per-action columns;
// today the details column is a JSON blob whose shape
// is per-action. Keeping details in JSON avoids a
// migration per new action type.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// AuditEvent is one row in the audit_events table.
// Action is the free-form action name (e.g.
// "rerun.create", "install.pause"). Actor is the
// identity of the actor (today the BOOP_DASHBOARD_TOKEN
// bearer; a future per-user identity replaces it).
// TargetID is the entity the action touched (a run id
// for re-runs, an installation id for pauses). Details
// is the per-action JSON blob (no schema validation —
// each caller knows its own shape).
type AuditEvent struct {
	ID         int64
	Action     string
	Actor      string
	TargetID   string
	OccurredAt time.Time
	Details    string
}

// ActorFromToken derives the audit-log actor string for
// a bearer token. The actor is the SHA-256 prefix of the
// token, namespaced by prefix ("dashboard:", "runner:")
// so a compliance view can split the two entry points in
// one query. Stable across requests (same token, same
// actor) and non-reversible (the raw token never lands in
// audit_events). Empty-token markers stay with the callers
// (dashboard:disabled / runner:unconfigured) because each
// endpoint has its own unauthenticated value.
func ActorFromToken(prefix, token string) string {
	sum := sha256.Sum256([]byte(token))
	return prefix + hex.EncodeToString(sum[:4])
}

// RecordAuditEvent appends a row to the audit_events
// table. Returns the new id so a caller can echo it in
// an HTTP response. An empty actor is rejected (a
// faceless action should not land in the log; a future
// "system" actor string is the right shape for
// non-human-initiated events).
func (s *Store) RecordAuditEvent(ctx context.Context, ev AuditEvent) (AuditEvent, error) {
	if ev.Actor == "" {
		return AuditEvent{}, fmt.Errorf("store: RecordAuditEvent: empty actor")
	}
	if ev.Action == "" {
		return AuditEvent{}, fmt.Errorf("store: RecordAuditEvent: empty action")
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (action, actor, target_id, occurred_at, details)
		VALUES (?, ?, ?, ?, ?)
	`,
		ev.Action, ev.Actor, nullString(ev.TargetID),
		ev.OccurredAt.UTC().Format(time.RFC3339Nano), nullString(ev.Details),
	)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: record audit event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: audit event id: %w", err)
	}
	ev.ID = id
	return ev, nil
}

// ListAuditEvents returns the most recent audit events,
// newest first, up to limit (1..500, default 100). The
// dashboard's "audit trail" view renders this as a
// feed with action / actor / target / time columns.
func (s *Store) ListAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, action, actor, COALESCE(target_id, ''),
			occurred_at, COALESCE(details, '')
		FROM audit_events
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var (
			ev AuditEvent
			t  string
		)
		if err := rows.Scan(&ev.ID, &ev.Action, &ev.Actor, &ev.TargetID, &t, &ev.Details); err != nil {
			return nil, fmt.Errorf("store: scan audit event: %w", err)
		}
		if tt, err := time.Parse(time.RFC3339Nano, t); err == nil {
			ev.OccurredAt = tt
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: audit events rows: %w", err)
	}
	return out, nil
}

// RetentionRow is one row in the "what is the
// retention-cutoff timer on this run?" view (QUB-112).
// The dashboard renders this on the run-detail page so
// the operator can answer "how long until this row is
// retention-pruned?" without grepping the receiver
// config.
type RetentionRow struct {
	RunID             string
	StartedAt         time.Time
	ScheduledDeletion time.Time
	RetentionDays     int
}

// ListRetentionSchedule returns every run with its
// scheduled-deletion timestamp. The schedule is
// started_at + retention; the receiver's actual
// PruneRuns runs on a 5-min tick so the actual delete
// can lag the scheduled time by up to one tick. The
// dashboard renders the scheduled time and the
// "imminent" flag (under 7 days) separately.
//
// An empty retention (0) means "use the default". The
// helper here returns the resolved value so the
// dashboard can render the days-out, not a config
// variable.
func (s *Store) ListRetentionSchedule(ctx context.Context, retention time.Duration) ([]RetentionRow, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	cutoff := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, started_at FROM runs WHERE started_at < ? ORDER BY started_at ASC
	`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: list retention schedule: %w", err)
	}
	defer rows.Close()
	var out []RetentionRow
	for rows.Next() {
		var (
			id string
			t  string
		)
		if err := rows.Scan(&id, &t); err != nil {
			return nil, fmt.Errorf("store: scan retention row: %w", err)
		}
		started, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			continue
		}
		out = append(out, RetentionRow{
			RunID:             id,
			StartedAt:         started,
			ScheduledDeletion: started.Add(retention),
			RetentionDays:     int(retention.Hours() / 24),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: retention rows: %w", err)
	}
	return out, nil
}

// MarshalDetails is a small helper for callers building
// the per-action JSON blob. Returns the JSON string
// (empty on error — the audit log should not lose an
// event because the details shape was malformed).
func MarshalDetails(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
