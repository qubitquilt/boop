package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Installation is one row in the /api/installations response. It
// mirrors the GitHub App installation object the receiver fetches
// via GET /app/installations; we keep only the fields the
// dashboard needs and add a fetched_at stamp so the cache TTL
// can be enforced even on rows that are never re-touched.
//
// Paused and LensOptOut are QUB-108/QUB-111 additions. Paused
// short-circuits webhook handling before claimJobSlot so an
// operator can mute a noisy install without uninstalling the
// App; LensOptOut is a JSON-encoded list of lens names that
// the runner should skip for this install (Phase 4 install
// controls). Both are written via SetInstallationControls;
// UpsertInstallations never overwrites them, so the
// background GitHub poll can't reset an operator's manual
// toggle.
type Installation struct {
	ID                  int64     `json:"id"`
	AccountLogin        string    `json:"account_login"`
	AccountType         string    `json:"account_type"`
	RepositorySelection string    `json:"repository_selection,omitempty"`
	InstalledAt         time.Time `json:"installed_at,omitempty"`
	FetchedAt           time.Time `json:"fetched_at"`
	Paused              bool      `json:"paused"`
	LensOptOut          []string  `json:"lens_opt_out"`
}

// UpsertInstallations replaces the installations table with the
// given list. We replace rather than merge so a repo that has
// uninstalled the App disappears from the dashboard
// immediately — keeping stale rows around would silently
// over-count the installed-repo KPI.
//
// The receiver calls this from a background poll (5-min cadence)
// rather than at request time, so the dashboard's GET is a cheap
// table read with no GitHub API call.
//
// QUB-108: paused and lens_opt_out are operator-controlled
// fields and must survive a GitHub poll. The naive
// "DELETE then INSERT" loses them, so we read the prior
// values for the incoming ids first and pass them back
// into the INSERT. The whole thing is one transaction so
// the operator's choice is never visible as a momentary
// default to a concurrent dashboard read.
func (s *Store) UpsertInstallations(ctx context.Context, installs []Installation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin install tx: %w", err)
	}
	defer tx.Rollback()

	// Collect the operator-controlled values for the ids
	// the poll is about to upsert, BEFORE the DELETE wipes
	// them. The map is keyed by id; missing ids get the
	// defaults. The dashboard only ever reads from the
	// final post-commit state, so this read inside the
	// transaction is invisible to concurrent readers.
	prior := make(map[int64]struct {
		paused     int
		lensOptRaw string
	}, len(installs))
	if len(installs) > 0 {
		ids := make([]any, 0, len(installs))
		placeholders := make([]string, 0, len(installs))
		for _, ins := range installs {
			ids = append(ids, ins.ID)
			placeholders = append(placeholders, "?")
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, paused, COALESCE(lens_opt_out, '[]') FROM installations WHERE id IN (`+
				strings.Join(placeholders, ",")+`)`, ids...)
		if err != nil {
			return fmt.Errorf("store: read prior controls: %w", err)
		}
		for rows.Next() {
			var id int64
			var p int
			var r string
			if err := rows.Scan(&id, &p, &r); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan prior controls: %w", err)
			}
			prior[id] = struct {
				paused     int
				lensOptRaw string
			}{p, r}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: prior controls rows: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM installations`); err != nil {
		return fmt.Errorf("store: clear installations: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO installations (
			id, account_login, account_type, repository_selection, installed_at, fetched_at,
			paused, lens_opt_out
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("store: prepare install insert: %w", err)
	}
	defer stmt.Close()
	for _, ins := range installs {
		var installedAt any
		if !ins.InstalledAt.IsZero() {
			installedAt = ins.InstalledAt.UTC().Format(time.RFC3339Nano)
		}
		// Respect the caller's FetchedAt when set; fall back to
		// now() so a caller that doesn't care still gets a
		// sensible row. The dashboard's TTL check reads this
		// back, so the caller is the one that knows when the
		// data was actually pulled from GitHub.
		fetchedAt := time.Now().UTC()
		if !ins.FetchedAt.IsZero() {
			fetchedAt = ins.FetchedAt.UTC()
		}
		// Default values for a row we never saw before this
		// poll: paused=0, lens_opt_out="[]". For a row we
		// DID see, copy the operator's choice.
		paused := 0
		lensOptRaw := "[]"
		if p, ok := prior[ins.ID]; ok {
			paused = p.paused
			lensOptRaw = p.lensOptRaw
		}
		if _, err := stmt.ExecContext(ctx,
			ins.ID, ins.AccountLogin, ins.AccountType, ins.RepositorySelection, installedAt,
			fetchedAt.Format(time.RFC3339Nano), paused, lensOptRaw,
		); err != nil {
			return fmt.Errorf("store: insert installation %d: %w", ins.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit installations: %w", err)
	}
	return nil
}

// ListInstallations returns all known installations ordered by
// account login (case-insensitive). Used by GET /api/installations.
func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_login, account_type,
			COALESCE(repository_selection, ''),
			COALESCE(installed_at, ''),
			fetched_at,
			paused,
			COALESCE(lens_opt_out, '[]')
		FROM installations
		ORDER BY LOWER(account_login) ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list installations: %w", err)
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var (
			ins        Installation
			installed  string
			fetchedAt  string
			repoSelect sql.NullString
			paused     int
			lensOptRaw string
		)
		if err := rows.Scan(&ins.ID, &ins.AccountLogin, &ins.AccountType, &repoSelect, &installed, &fetchedAt, &paused, &lensOptRaw); err != nil {
			return nil, fmt.Errorf("store: scan installation: %w", err)
		}
		ins.Paused = paused != 0
		ins.LensOptOut = decodeLensOptOut(lensOptRaw)
		ins.RepositorySelection = repoSelect.String
		if installed != "" {
			if t, err := time.Parse(time.RFC3339Nano, installed); err == nil {
				ins.InstalledAt = t
			}
		}
		if fetchedAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, fetchedAt); err == nil {
				ins.FetchedAt = t
			}
		}
		out = append(out, ins)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: installation rows: %w", err)
	}
	return out, nil
}

// SetInstallationControls writes the operator-controlled
// fields (paused, lens_opt_out) for a single installation.
// Used by Phase 4's /dashboard/installations/{id} POST. The
// GitHub background poller does not call this; only the
// dashboard does, so the operator's choice survives the next
// poll. A nil/empty lens list is stored as the JSON literal
// "[]" so the column's NOT NULL DEFAULT holds even on
// install rows that pre-date the schema bump.
func (s *Store) SetInstallationControls(ctx context.Context, id int64, paused bool, lensOptOut []string) error {
	lens := lensOptOut
	if lens == nil {
		lens = []string{}
	}
	raw, err := encodeLensOptOut(lens)
	if err != nil {
		return fmt.Errorf("store: encode lens_opt_out: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE installations SET paused = ?, lens_opt_out = ? WHERE id = ?
	`, boolToInt(paused), raw, id)
	if err != nil {
		return fmt.Errorf("store: set install controls: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set install controls rows: %w", err)
	}
	if n == 0 {
		// The install might not be in the table yet (poll
		// hasn't caught up). Treat as a non-fatal no-op so
		// the dashboard doesn't have to retry on a 5-min
		// poll cadence.
		return nil
	}
	return nil
}

// GetInstallation returns a single installation by id, or
// sql.ErrNoRows if absent. Used by the dashboard's install
// detail page.
func (s *Store) GetInstallation(ctx context.Context, id int64) (Installation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, account_login, account_type,
			COALESCE(repository_selection, ''),
			COALESCE(installed_at, ''),
			fetched_at,
			paused,
			COALESCE(lens_opt_out, '[]')
		FROM installations WHERE id = ?
	`, id)
	var (
		ins        Installation
		installed  string
		fetchedAt  string
		repoSelect sql.NullString
		paused     int
		lensOptRaw string
	)
	if err := row.Scan(&ins.ID, &ins.AccountLogin, &ins.AccountType, &repoSelect, &installed, &fetchedAt, &paused, &lensOptRaw); err != nil {
		return Installation{}, err
	}
	ins.Paused = paused != 0
	ins.LensOptOut = decodeLensOptOut(lensOptRaw)
	ins.RepositorySelection = repoSelect.String
	if installed != "" {
		if t, err := time.Parse(time.RFC3339Nano, installed); err == nil {
			ins.InstalledAt = t
		}
	}
	if fetchedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, fetchedAt); err == nil {
			ins.FetchedAt = t
		}
	}
	return ins, nil
}

// IsInstallationPaused is the webhook hot-path check. It
// returns true if the installation is explicitly paused. A
// missing row (poll hasn't run yet) returns false, so a
// newly-installed repo is never silently muted by a stale
// read.
func (s *Store) IsInstallationPaused(ctx context.Context, id int64) (bool, error) {
	var paused int
	err := s.db.QueryRowContext(ctx, `SELECT paused FROM installations WHERE id = ?`, id).Scan(&paused)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: is install paused: %w", err)
	}
	return paused != 0, nil
}

// encodeLensOptOut / decodeLensOptOut are the JSON
// marshaling helpers for the lens_opt_out TEXT column. The
// column is a TEXT NOT NULL DEFAULT '[]' so the only thing
// that ever lives in it is a JSON array of lens names. We
// tolerate a malformed cell (e.g. legacy data from before
// the column existed) by returning an empty slice — the
// dashboard renders "no opt-outs" rather than crashing on
// the first row.
func encodeLensOptOut(lens []string) (string, error) {
	if lens == nil {
		lens = []string{}
	}
	b, err := json.Marshal(lens)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeLensOptOut(raw string) []string {
	if raw == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LatestInstallationFetch returns the FetchedAt of the most
// recently stored installation row, or zero time if the table
// is empty. The background poller uses this to skip a GitHub API
// call when the cache is fresh.
func (s *Store) LatestInstallationFetch(ctx context.Context) (time.Time, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(fetched_at) FROM installations`).Scan(&v)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: latest install fetch: %w", err)
	}
	if !v.Valid || v.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse latest fetch: %w", err)
	}
	return t, nil
}
