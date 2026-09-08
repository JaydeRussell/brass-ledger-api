// Package db owns this service's connection to Postgres and its schema
// migrations. Feature-specific query code lives in its own package
// instead of here (internal/user, internal/bcpcache) — this package
// stays a thin, feature-agnostic base every one of them builds on.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New opens a connection pool to Postgres and verifies it's reachable
// before returning, so a misconfigured DATABASE_URL fails at startup
// with a clear error instead of surfacing as a mysterious request-time
// failure later.
func New(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	return pool, nil
}
