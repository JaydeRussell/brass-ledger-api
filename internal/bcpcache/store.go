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
	"time"

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

// GetFresh is Get with a maximum age, for the durable values that aren't
// permanent — see bcp.DurableCache's doc comment. It also returns the
// row's cached_at so the caller can seed an in-memory entry with the
// age the data actually has, rather than restarting its clock (which
// would both hide staleness and make the "last updated" timestamp shown
// to the user wrong).
//
// A row older than maxAge reports found=false, exactly like a missing
// one: the caller's next step is a real fetch either way. The row is
// deliberately left in place rather than deleted — a successful refetch
// overwrites it via Set, and if the refetch fails the stale row is
// better kept than thrown away.
func (s *Store) GetFresh(ctx context.Context, key string, version int, maxAge time.Duration, dest any) (bool, time.Time, error) {
	var raw []byte
	var cachedAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT data, cached_at FROM bcp_durable_cache WHERE cache_key = $1 AND cache_version = $2`, key, version,
	).Scan(&raw, &cachedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, fmt.Errorf("reading bcp_durable_cache[%s]: %w", key, err)
	}
	if maxAge > 0 && time.Since(cachedAt) > maxAge {
		return false, cachedAt, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, cachedAt, fmt.Errorf("decoding bcp_durable_cache[%s]: %w", key, err)
	}
	return true, cachedAt, nil
}

// GetMany reads every key present in one query, returning the raw JSON
// for each hit. Keys with no row (or a row at a different version) are
// simply absent from the result — the caller falls back to its normal
// per-key path for those. Decoding is left to the caller because a
// single call site can be reading several different value types.
//
// This is what keeps a cold stats page from doing one SELECT per event
// the player has ever attended: see bcp.Client's Prewarm* methods.
func (s *Store) GetMany(ctx context.Context, keys []string, version int) (map[string]json.RawMessage, error) {
	found := make(map[string]json.RawMessage, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT cache_key, data FROM bcp_durable_cache WHERE cache_key = ANY($1) AND cache_version = $2`,
		keys, version,
	)
	if err != nil {
		return nil, fmt.Errorf("reading %d bcp_durable_cache keys: %w", len(keys), err)
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("scanning bcp_durable_cache row: %w", err)
		}
		found[key] = json.RawMessage(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading bcp_durable_cache rows: %w", err)
	}
	return found, nil
}

// Delete removes a cached row. Used when a user explicitly asks for
// fresh data (see bcp.Client.InvalidatePlayerEventHistory) — without it,
// dropping the in-memory entry would just reload the same stale value
// from here on the very next read. Deleting a key that isn't there isn't
// an error: the caller's desired end state already holds.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM bcp_durable_cache WHERE cache_key = $1`, key); err != nil {
		return fmt.Errorf("deleting bcp_durable_cache[%s]: %w", key, err)
	}
	return nil
}
