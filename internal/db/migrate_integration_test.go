//go:build integration

// Migrate against a real Postgres. The pure parts (which migrations are
// pending, the filename rules) are unit-tested in migrate_test.go; what
// needs a database is the one behaviour that can't be faked —
// concurrency. Gated behind the "integration" build tag, same as
// internal/user and internal/bcpcache. Run with:
//
//	go test -tags=integration ./internal/db/...
package db

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connecting to database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestMigrate_ConcurrentCallersOnAFreshDatabase is the regression test
// for a race that took a CI run down.
//
// Migrating is not safe to do concurrently without a lock: CREATE TABLE
// IF NOT EXISTS is not atomic against another session creating the same
// table (Postgres raises a duplicate-key error on pg_type's own index),
// and two callers that both read an empty schema_migrations will both
// decide every migration is pending and both try to apply it — producing
// a duplicate key on schema_migrations_pkey, or "column already exists"
// from whichever bare ALTER TABLE they collide on.
//
// It only shows up against a *fresh* database, which is why it survived
// local testing and only appeared in CI: a developer's database is
// already migrated, so there's nothing left to race over. This test
// makes its own empty database so it reproduces the real thing
// everywhere.
func TestMigrate_ConcurrentCallersOnAFreshDatabase(t *testing.T) {
	adminURL := os.Getenv("DATABASE_URL")
	if adminURL == "" {
		t.Fatal("DATABASE_URL must be set to run integration tests — see this file's build-tag comment")
	}

	ctx := context.Background()
	admin := testPool(t, adminURL)

	// A database of its own, so "fresh" is guaranteed rather than assumed.
	name := fmt.Sprintf("migrate_race_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating a scratch database: %v", err)
	}
	t.Cleanup(func() {
		// Connections to it must be gone first; the pools below are
		// closed by their own t.Cleanup, which runs before this one.
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("dropping scratch database %s: %v", name, err)
		}
	})

	scratchURL := replaceDatabaseName(adminURL, name)

	// Separate pools, as separate processes would have — one shared pool
	// could mask the race by serialising on a single connection.
	const callers = 4
	pools := make([]*pgxpool.Pool, callers)
	for i := range callers {
		pools[i] = testPool(t, scratchURL)
	}

	start := make(chan struct{})
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // line them all up so they collide
			errs[i] = Migrate(ctx, pools[i])
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate #%d failed: %v", i, err)
		}
	}

	// Every migration applied exactly once, despite four callers all
	// seeing an empty schema_migrations at the same moment.
	var recorded int
	if err := pools[0].QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("counting applied migrations: %v", err)
	}
	applied, err := appliedVersions(ctx, mustAcquire(t, pools[0]))
	if err != nil {
		t.Fatalf("reading applied migrations: %v", err)
	}
	if recorded != len(applied) {
		t.Errorf("schema_migrations has %d rows but %d distinct versions — a migration was recorded twice", recorded, len(applied))
	}
	paths, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("listing embedded migrations: %v", err)
	}
	if pending := pendingMigrations(paths, applied); len(pending) != 0 {
		t.Errorf("after migrating, %d migrations still report as pending: %v", len(pending), pending)
	}
}

func mustAcquire(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	t.Cleanup(conn.Release)
	return conn
}

// replaceDatabaseName swaps the database out of a postgres:// URL,
// keeping credentials, host and query string. Deliberately string
// surgery on the last path segment rather than a net/url parse: the
// DATABASE_URL shapes this project actually uses (docker-compose, CI,
// Neon) all have exactly one path segment.
func replaceDatabaseName(rawURL, name string) string {
	base, query, hasQuery := strings.Cut(rawURL, "?")
	slash := strings.LastIndexByte(base, '/')
	out := base[:slash+1] + name
	if hasQuery {
		out += "?" + query
	}
	return out
}
