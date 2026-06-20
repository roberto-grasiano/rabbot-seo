package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// loadAppliedVersions reads the set of already-applied migration versions. It
// fully drains and closes the rows before returning so the single write
// connection is free for the migration transaction that follows.
func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return applied, nil
}

// runMigrations applies any not-yet-applied embedded migrations in lexical
// order, tracking applied versions in schema_migrations. Each migration is
// applied in its own transaction (the migration body plus its
// schema_migrations row commit together); they are not all wrapped in one tx.
func runMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMP NOT NULL
		)`); err != nil {
		return err
	}

	applied, err := loadAppliedVersions(ctx, db)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := parseVersion(name)
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: migration %s: %w", ErrMigrationDirty, name, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			// Bind the UTC time.Time directly (matching the column's TIMESTAMP
			// affinity and the rest of the schema) rather than an RFC3339 string.
			version, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// parseVersion extracts the leading zero-padded integer from "0001_init.sql".
func parseVersion(name string) (int, error) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, fmt.Errorf("%w: bad migration name %q", ErrMigrationDirty, name)
	}
	return strconv.Atoi(name[:idx])
}
