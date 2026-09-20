package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS embeds every .sql file in migrations/ into the built
// binary, so a deploy is just the binary — no separate SQL files need to
// ship or be mounted alongside it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every embedded migration that hasn't run yet, in
// filename order (hence the 0001_, 0002_... prefixes — name new ones
// accordingly), each inside its own transaction. Deliberately minimal —
// no rollback/down migrations, no external migration tool — since this
// project's schema is still small; reach for a real migration library if
// that stops being true.
//
// The set of already-applied versions is read in one query rather than
// one "has this run yet?" round trip per file. On a warm database every
// migration is a no-op, so that per-file check was the entire cost of
// this function: 15 sequential round trips to Postgres, on every single
// process start, to learn that there was nothing to do. This runs before
// the HTTP listener starts (see cmd/server/main.go), and the container
// this deploys to sleeps after ten minutes idle, so "every process
// start" means most real visits — it was pure cold-start latency.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	paths, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("listing embedded migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	for _, path := range pendingMigrations(paths, applied) {
		if err := applyMigration(ctx, pool, path); err != nil {
			return err
		}
	}

	return nil
}

// appliedVersions reads every version already recorded in
// schema_migrations. Reading the whole column is fine at this schema's
// size (one short row per migration ever written) and is what replaces
// the per-file existence check.
func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]struct{}, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("listing applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]struct{})
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	return applied, nil
}

// pendingMigrations returns the embedded paths that still need applying,
// in filename order. Pure — no database — so the ordering and
// already-applied rules are testable without one (see migrate_test.go).
//
// The embedded path itself (e.g. "migrations/0001_users_and_sessions.sql")
// is the version id — stable, unique, and self-documenting in the
// schema_migrations table. That also means renaming an applied migration
// file makes it run a second time; see migrate_test.go's
// TestMigrationsHaveUniqueNumericPrefixes for why that matters.
func pendingMigrations(paths []string, applied map[string]struct{}) []string {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)

	pending := make([]string, 0, len(sorted))
	for _, path := range sorted {
		if _, done := applied[path]; done {
			continue
		}
		pending = append(pending, path)
	}
	return pending
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, path string) error {
	sqlBytes, err := migrationsFS.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading migration %s: %w", path, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction for migration %s: %w", path, err)
	}
	defer tx.Rollback(ctx) // no-op once committed below

	if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("applying migration %s: %w", path, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, path); err != nil {
		return fmt.Errorf("recording migration %s: %w", path, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing migration %s: %w", path, err)
	}

	return nil
}
