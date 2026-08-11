package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is a small chained builder for a single schema
// version's DDL (RF-012). Each migration block (migrateV1,
// migrateV2, ...) used to inline three loops:
//
//   - a `for i, stmt := range stmts` over CREATE TABLE / INDEX
//     strings, each with its own error wrap
//   - a `for _, c := range colAdds` over column additions, each
//     guarded by a hasColumn pragma check
//   - another `for i, stmt := range stmts` over CREATE INDEX
//     strings
//
// The builder collapses all three into a single chain:
// every step returns *migration, and the first error
// short-circuits the rest (a "fail fast" chain). The chain
// reads as a declarative list of operations, which is the
// shape the audit table next to each migration already
// implies (the comment block names the table + index and
// the dashboard query it feeds).
//
// The chain is the right shape because every step is
// commutative w.r.t. failure isolation: a CREATE TABLE
// failure should abort the migration (a missing table makes
// the next ADD COLUMN invalid), but the operations within
// each step are independent (a CREATE INDEX failure on
// idx_audit_events_actor should not roll back the table
// creation, which is what already happened).
//
// The migration's name is included in every error so a
// failed applyMigrations surfaces the version that broke
// without the operator having to read the framework's
// outer wrap ("migration v3: ...").
type migration struct {
	ctx  context.Context
	db   *sql.DB
	name string
	err  error
}

func newMigration(ctx context.Context, db *sql.DB, name string) *migration {
	return &migration{ctx: ctx, db: db, name: name}
}

// createTable runs a CREATE TABLE statement. The caller is
// responsible for including IF NOT EXISTS so a partial
// upgrade that crashed mid-way retries cleanly on the next
// Open (the framework's contract).
func (m *migration) createTable(stmt string) *migration {
	if m.err != nil {
		return m
	}
	if _, err := m.db.ExecContext(m.ctx, stmt); err != nil {
		m.err = fmt.Errorf("%s create table: %w", m.name, err)
	}
	return m
}

// createIndex runs a CREATE INDEX statement. The caller
// includes IF NOT EXISTS (matching createTable's contract).
func (m *migration) createIndex(stmt string) *migration {
	if m.err != nil {
		return m
	}
	if _, err := m.db.ExecContext(m.ctx, stmt); err != nil {
		m.err = fmt.Errorf("%s create index: %w", m.name, err)
	}
	return m
}

// addColumn runs an ALTER TABLE ADD COLUMN with a hasColumn
// guard. SQLite has no portable ADD COLUMN IF NOT EXISTS,
// so the only idempotent path is the pragma check. The
// guard is internal to the builder; the caller just hands
// over the table / column / definition triple.
func (m *migration) addColumn(table, col, def string) *migration {
	if m.err != nil {
		return m
	}
	has, err := hasColumn(m.ctx, m.db, table, col)
	if err != nil {
		m.err = fmt.Errorf("%s hasColumn %s.%s: %w", m.name, table, col, err)
		return m
	}
	if has {
		return m
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def)
	if _, err := m.db.ExecContext(m.ctx, stmt); err != nil {
		m.err = fmt.Errorf("%s alter %s.%s: %w", m.name, table, col, err)
	}
	return m
}

// migrationError returns the accumulated error, or nil if
// every step succeeded. The framework wraps this with the
// migration version ("migration v3: ...") so the operator
// sees both the builder-internal context (which step) and
// the framework context (which version).
func (m *migration) migrationError() error {
	return m.err
}
