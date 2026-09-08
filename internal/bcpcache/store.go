// Package bcpcache is the Postgres-backed implementation of
// bcp.DurableCache (see internal/bcp/durable.go for the interface and
// exactly what gets written here and why, and
// internal/db/migrations/0004_bcp_durable_cache.sql for the schema).
// Kept as its own package, the same way internal/user is Postgres logic
// separate from internal/auth, so internal/bcp itself never needs to
// import pgx.
package bcpcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store implements bcp.DurableCache against a single generic
// key-value table.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an existing connection pool (see internal/db) as a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Get decodes the stored JSON for key into dest (a pointer) and reports
// whether a value was found at all. A missing key is not an error.
func (s *Store) Get(ctx context.Context, key string, dest any) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT data FROM bcp_durable_cache WHERE cache_key = $1`, key,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("reading bcp_durable_cache[%s]: %w", key, err)
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, fmt.Errorf("decoding bcp_durable_cache[%s]: %w", key, err)
	}
	return true, nil
}

// Set marshals value as JSON and stores it under key, overwriting
// whatever (if anything) was there before.
func (s *Store) Set(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding bcp_durable_cache[%s]: %w", key, err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO bcp_durable_cache (cache_key, data, cached_at)
		VALUES ($1, $2, now())
		ON CONFLICT (cache_key) DO UPDATE SET data = EXCLUDED.data, cached_at = EXCLUDED.cached_at
	`, key, raw); err != nil {
		return fmt.Errorf("writing bcp_durable_cache[%s]: %w", key, err)
	}
	return nil
}
