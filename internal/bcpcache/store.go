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
// whether a value was found at all — false if nothing is stored under
// key, or if something is but tagged with a different cache_version
// than version (a row written under an older schema; see
// bcp.CacheSchemaVersion). A miss either way is not an error.
func (s *Store) Get(ctx context.Context, key string, version int, dest any) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT data FROM bcp_durable_cache WHERE cache_key = $1 AND cache_version = $2`, key, version,
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

// Set marshals value as JSON and stores it under key tagged with
// version, overwriting whatever (if anything, at whatever version) was
// there before.
func (s *Store) Set(ctx context.Context, key string, version int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding bcp_durable_cache[%s]: %w", key, err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO bcp_durable_cache (cache_key, data, cache_version, cached_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (cache_key) DO UPDATE SET
			data = EXCLUDED.data,
			cache_version = EXCLUDED.cache_version,
			cached_at = EXCLUDED.cached_at
	`, key, raw, version); err != nil {
		return fmt.Errorf("writing bcp_durable_cache[%s]: %w", key, err)
	}
	return nil
}
