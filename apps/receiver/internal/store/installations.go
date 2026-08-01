package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Installation is one row in the /api/installations response. It
// mirrors the GitHub App installation object the receiver fetches
// via GET /app/installations; we keep only the fields the
// dashboard needs and add a fetched_at stamp so the cache TTL
// can be enforced even on rows that are never re-touched.
type Installation struct {
	ID                  int64     `json:"id"`
	AccountLogin        string    `json:"account_login"`
	AccountType         string    `json:"account_type"`
	RepositorySelection string    `json:"repository_selection,omitempty"`
	InstalledAt         time.Time `json:"installed_at,omitempty"`
	FetchedAt           time.Time `json:"fetched_at"`
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
func (s *Store) UpsertInstallations(ctx context.Context, installs []Installation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin install tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM installations`); err != nil {
		return fmt.Errorf("store: clear installations: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO installations (
			id, account_login, account_type, repository_selection, installed_at, fetched_at
		) VALUES (?, ?, ?, ?, ?, ?)
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
		if _, err := stmt.ExecContext(ctx,
			ins.ID, ins.AccountLogin, ins.AccountType, ins.RepositorySelection, installedAt,
			fetchedAt.Format(time.RFC3339Nano),
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
			fetched_at
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
		)
		if err := rows.Scan(&ins.ID, &ins.AccountLogin, &ins.AccountType, &repoSelect, &installed, &fetchedAt); err != nil {
			return nil, fmt.Errorf("store: scan installation: %w", err)
		}
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
