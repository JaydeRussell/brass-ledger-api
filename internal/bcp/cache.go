package bcp

import (
	"context"
	"sync"
	"time"
)

// minRefetchInterval mirrors the frontend's original client-side cache:
// see CLAUDE.md's "be respectful of third-party APIs" rule — this is an
// unofficial endpoint, so a minimum interval is enforced between real
// network requests per key. Moving this server-side is the actual point
// of this package: every user of this app now shares one cache and one
// rate limit against BCP, instead of each browser tab enforcing its own.
const minRefetchInterval = 60 * time.Second

// minManualInvalidateInterval is a much shorter floor than
// minRefetchInterval, applied only to Invalidate (an explicit "check
// again now" request, e.g. a user-facing Refresh button) — not to blunt
// legitimate use, just to stop a rapidly double-clicked button from
// turning into back-to-back real BCP requests. Deliberately loose (2s,
// not minRefetchInterval's full 60s) since a manual refresh is a
// deliberate one-off action, not the kind of routine/automatic traffic
// the no-polling rule is really guarding against — see CLAUDE.md.
// Frontend's RefreshButton (app/components/shared/refreshButton.tsx in
// brass-ledger-web) mirrors this exact value for its own cooldown
// display; the two are kept in sync by hand since they're separate
// repos. A var rather than a const purely so cache_test.go can shrink it
// to make the "the floor actually elapses" case fast and deterministic
// instead of sleeping for 2 real seconds — production behavior is
// unaffected.
var minManualInvalidateInterval = 2 * time.Second

type cacheEntry[T any] struct {
	data      T
	fetchedAt time.Time
}

// inflight represents a fetch already in progress for a given key — a
// second caller for the same key waits on `done` instead of starting a
// duplicate request, the same "in-flight de-dup" the frontend's original
// makeCache did.
type inflight[T any] struct {
	done   chan struct{}
	result T
	err    error
}

// Cache is a generic, concurrency-safe, TTL'd, de-duplicated cache in
// front of one BCP fetch function — one instance per distinct kind of
// BCP call (event info, players, a round's pairings, etc.), each keyed
// by whatever identifies that call (an event id, an "eventId:type:round"
// tuple, and so on).
type Cache[T any] struct {
	mu       sync.Mutex
	entries  map[string]cacheEntry[T]
	inFlight map[string]*inflight[T]
	fetch    func(ctx context.Context, key string) (T, error)
}

// NewCache builds a Cache backed by fetch — fetch is called at most once
// per key per minRefetchInterval, with concurrent callers for the same
// key sharing a single underlying call.
func NewCache[T any](fetch func(ctx context.Context, key string) (T, error)) *Cache[T] {
	return &Cache[T]{
		entries:  make(map[string]cacheEntry[T]),
		inFlight: make(map[string]*inflight[T]),
		fetch:    fetch,
	}
}

// Get returns the cached value for key, fetching (or joining an
// in-progress fetch) if it's missing or stale. A failed fetch is never
// cached, so the next call tries again rather than being stuck serving
// an error for a full minRefetchInterval.
func (c *Cache[T]) Get(ctx context.Context, key string) (T, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && time.Since(e.fetchedAt) < minRefetchInterval {
		c.mu.Unlock()
		return e.data, nil
	}
	if inf, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		<-inf.done
		return inf.result, inf.err
	}

	inf := &inflight[T]{done: make(chan struct{})}
	c.inFlight[key] = inf
	c.mu.Unlock()

	data, err := c.fetch(ctx, key)

	c.mu.Lock()
	if err == nil {
		c.entries[key] = cacheEntry[T]{data: data, fetchedAt: time.Now()}
	}
	delete(c.inFlight, key)
	c.mu.Unlock()

	inf.result, inf.err = data, err
	close(inf.done)

	return data, err
}

// Invalidate clears key's cached entry (if fetched more than
// minManualInvalidateInterval ago — a very recent entry is left alone,
// see that const's doc comment), so the next Get performs a real fetch
// regardless of the normal minRefetchInterval TTL. Meant for an explicit,
// user-initiated "check again now" action — e.g. a signed-in account's
// own event list, which can go stale in ways only they'd know to ask
// about (registering for something new) — not for routine use.
func (c *Cache[T]) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && time.Since(e.fetchedAt) < minManualInvalidateInterval {
		return
	}
	delete(c.entries, key)
}

// FetchedAt reports when key's cached entry was last actually fetched
// from BCP, without triggering a fetch itself — the "last updated"
// timestamp a caller can surface to the frontend. ok is false if there's
// no cached entry at all yet (nothing has ever been fetched for key).
func (c *Cache[T]) FetchedAt(key string) (t time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e.fetchedAt, ok
}
