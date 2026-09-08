package bcp

import "context"

// DurableCache is the persistence layer a Client optionally writes
// permanently-cacheable BCP responses to and reads them back from —
// implemented against Postgres by internal/bcpcache (see
// internal/db/migrations/0004_bcp_durable_cache.sql), kept as a small
// interface here so this package doesn't need to depend on pgx directly.
//
// A nil DurableCache (the default — NewClient/NewClientWithBaseURL don't
// set one) just means every fetch behaves exactly as it always has: real
// BCP calls, deduped/rate-limited only by the in-memory Cache in
// cache.go. Every fetch* function in client.go that supports durable
// caching checks for nil before touching it, so this is entirely
// optional and every existing test (which never sets one) is unaffected.
//
// Get decodes the stored JSON into dest (a pointer) and reports whether
// a value was found at all. Set marshals value as JSON and stores it —
// callers only ever call Set once they've confirmed the underlying BCP
// data can never change again (an already-concluded event's info,
// roster, pairings, placings; a league's gw_itc/hobby classification,
// which is effectively permanent). Because of that, anything found here
// is trusted unconditionally on read — there's no TTL or "is this still
// valid" check, unlike the in-memory Cache.
type DurableCache interface {
	Get(ctx context.Context, key string, dest any) (bool, error)
	Set(ctx context.Context, key string, value any) error
}

// SetDurableCache installs c's durable cache. Call once, right after
// NewClient/NewClientWithBaseURL, before any fetches happen — it's not
// safe to swap concurrently with in-flight requests.
func (c *Client) SetDurableCache(d DurableCache) {
	c.durable = d
}
