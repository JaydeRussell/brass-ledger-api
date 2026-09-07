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
	sort.Strings(paths)

	for _, path := range paths {
		if err := applyMigrationIfNeeded(ctx, pool, path); err != nil {
			return err
		}
	}

	return nil
}

func applyMigrationIfNeeded(ctx context.Context, pool *pgxpool.Pool, path string) error {
	// The embedded path itself (e.g. "migrations/0001_users_and_sessions.sql")
	// is the version id — stable, unique, and self-documenting in the
	// schema_migrations table.
	var alreadyApplied bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, path,
	).Scan(&alreadyApplied); err != nil {
		return fmt.Errorf("checking migration %s: %w", path, err)
	}
	if alreadyApplied {
		return nil
	}

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
