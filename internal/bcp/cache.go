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

// MinRefetchInterval is minRefetchInterval, exported so the HTTP layer
// can tell a browser how long this service will keep answering a
// question the same way — see internal/api's cacheFor. Kept as a
// derived constant rather than making minRefetchInterval itself
// exported so the package's own code keeps reading the same name it
// always has.
const MinRefetchInterval = minRefetchInterval

// EndedEventTTL is how long a response derived entirely from an
// already-concluded event may be reused. A concluded event's info,
// roster, pairings and placings are immutable — it is what the durable
// cache stores permanently — so the only real bound is how long we want
// to be unable to correct a mistake.
const EndedEventTTL = 24 * time.Hour

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

// goneTTL is how long a Cache remembers that BCP said a key does not
// exist (404/410 — see IsGone), so a lookup that can never succeed
// doesn't cost a real request every time someone loads the page.
//
// This matters more than it sounds. A BCP registration can outlive the
// event it points at: the organizer deletes the event, the player's
// registration list still names it. /api/me/events resolves every
// not-yet-concluded registration through FetchEventInfo, tolerates the
// failure, and drops the event from the response — so nothing looked
// broken, and two dead registrations quietly cost one BCP round trip on
// every single request. Measured 2026-09-20: warm /api/me/events was
// 731ms in production against /api/me/stats' 231ms, entirely this.
//
// An hour, not longer, and deliberately in memory only — never durable.
// A 404 is the one status that reads as permanent, but "permanent"
// here is still an inference from someone else's API, and writing it to
// Postgres would let one bad hour outlive the process, the deploy, and
// any chance of noticing. In memory it expires on its own, and a
// container that sleeps after ten minutes idle forgets it sooner than
// that anyway.
//
// A var rather than a const purely so cache_test.go can shrink it and
// assert the expiry without sleeping for an hour — the same trick
// minManualInvalidateInterval uses above. Production behavior is
// unaffected.
var goneTTL = time.Hour

// goneEntry records a fetch that failed in a way that won't get better.
type goneEntry struct {
	err       error
	recordedA time.Time
}

type cacheEntry[T any] struct {
	data      T
	fetchedAt time.Time
	// This entry's own lifetime. Usually the Cache's default, but a
	// cache built with NewCacheWithValueTTL can give each entry a
	// lifetime derived from the value itself — see eventInfoTTL in
	// client.go, where an event starting next month is worth holding on
	// to far longer than one currently being played.
	ttl time.Duration
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
	// Keys BCP has said don't exist, and when it said so. Separate from
	// entries because there is no value to hold — only the error to
	// replay. See goneTTL.
	gone  map[string]goneEntry
	fetch func(ctx context.Context, key string) (T, error)
	ttl   time.Duration
	// Optional. Given a freshly fetched value, returns how long it's
	// worth keeping; a non-positive result falls back to ttl.
	ttlFor func(T) time.Duration
}

// NewCache builds a Cache backed by fetch, using the default
// minRefetchInterval TTL — fetch is called at most once per key per
// minRefetchInterval, with concurrent callers for the same key sharing a
// single underlying call. Use NewCacheWithTTL instead for a BCP call
// whose data goes stale on a meaningfully different schedule (see that
// constructor's doc comment).
func NewCache[T any](fetch func(ctx context.Context, key string) (T, error)) *Cache[T] {
	return NewCacheWithTTL(fetch, minRefetchInterval)
}

// NewCacheWithTTL builds a Cache backed by fetch with a custom TTL,
// for a BCP call whose result goes stale far slower (or faster) than the
// default minRefetchInterval — e.g. a signed-in account's own event
// registrations/placings history, which only changes when they register
// for something new or an event concludes, not on every page load the
// way live pairings/standings do. Concurrent callers for the same key
// still share a single underlying fetch, exactly as NewCache.
func NewCacheWithTTL[T any](fetch func(ctx context.Context, key string) (T, error), ttl time.Duration) *Cache[T] {
	return NewCacheWithValueTTL(fetch, ttl, nil)
}

// NewCacheWithValueTTL builds a Cache whose entries can each live for a
// different length of time, decided by ttlFor from the value itself.
// Pass nil for ttlFor to get a plain fixed-TTL cache.
//
// This exists because "how long is this still true?" isn't always a
// property of the *kind* of thing fetched, but of the particular thing:
// an event starting in six weeks isn't going to change in the next hour,
// while one being played right now changes every round. Caching both for
// the same 60 seconds means either re-fetching the first pointlessly or
// serving the second stale — see eventInfoTTL in client.go.
//
// ttlFor is called while the cache's lock is held, so it must be quick
// and must not call back into the cache.
func NewCacheWithValueTTL[T any](
	fetch func(ctx context.Context, key string) (T, error),
	ttl time.Duration,
	ttlFor func(T) time.Duration,
) *Cache[T] {
	return &Cache[T]{
		entries:  make(map[string]cacheEntry[T]),
		inFlight: make(map[string]*inflight[T]),
		gone:     make(map[string]goneEntry),
		fetch:    fetch,
		ttl:      ttl,
		ttlFor:   ttlFor,
	}
}

// ttlOf is this cache's lifetime for a particular value.
func (c *Cache[T]) ttlOf(v T) time.Duration {
	if c.ttlFor != nil {
		if d := c.ttlFor(v); d > 0 {
			return d
		}
	}
	return c.ttl
}

// Get returns the cached value for key, fetching (or joining an
// in-progress fetch) if it's missing or stale.
//
// A failed fetch is not cached — the next call tries again rather than
// being stuck serving an error for a full TTL — with one exception: if
// BCP said the key does not exist (IsGone), that answer is held for
// goneTTL and replayed without a request. Retrying a 404 every time is
// not resilience, it's a permanent per-request tax on data that will
// never arrive; see goneTTL for the bug that made the difference
// measurable.
func (c *Cache[T]) Get(ctx context.Context, key string) (T, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && time.Since(e.fetchedAt) < e.ttl {
		c.mu.Unlock()
		return e.data, nil
	}
	if g, ok := c.gone[key]; ok && time.Since(g.recordedA) < goneTTL {
		c.mu.Unlock()
		var zero T
		return zero, g.err
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
	switch {
	case err == nil:
		c.entries[key] = cacheEntry[T]{data: data, fetchedAt: time.Now(), ttl: c.ttlOf(data)}
		// A key that resolves is no longer gone — covers an event that
		// 404s while an organizer is mid-edit and comes back.
		delete(c.gone, key)
	case IsGone(err):
		c.gone[key] = goneEntry{err: err, recordedA: time.Now()}
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
//
// Reports whether it actually cleared anything, so a caller backing this
// cache with a durable one knows whether to drop that copy too — see
// Client.InvalidatePlayerEventHistory. Dropping the durable row on a
// throttled call would defeat the throttle, since the next read would go
// to BCP instead of Postgres. Callers that don't care can ignore it.
func (c *Cache[T]) Invalidate(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && time.Since(e.fetchedAt) < minManualInvalidateInterval {
		return false
	}
	delete(c.entries, key)
	// An explicit "check again now" drops the gone marker as well: the
	// whole point of the button is that the user knows something we
	// inferred is out of date, and a resurrected event is precisely
	// that. Not throttled separately — the entries check above already
	// gates the request rate, and a gone key has no entry to gate.
	delete(c.gone, key)
	return true
}

// Put seeds an entry directly, with the time the data was really
// obtained rather than "now". Used to hand the cache a value loaded from
// the durable Postgres cache (see history.go) so that, for the rest of
// this process's life, it behaves exactly like one fetched normally:
// it ages out of the TTL on schedule, and FetchedAt reports the truth.
//
// Seeding with time.Now() instead would restart the clock on data that
// might be nearly TTL-old already, and would make the "last updated"
// timestamp surfaced to the user (see internal/api/me.go's
// UpcomingFetchedAt) claim a fetch that never happened.
func (c *Cache[T]) Put(key string, data T, fetchedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry[T]{data: data, fetchedAt: fetchedAt, ttl: c.ttlOf(data)}
	delete(c.gone, key)
}

// Fresh reports whether key has an entry Get would actually still serve
// — present *and* within the TTL.
//
// Deliberately distinct from FetchedAt, which reports only presence: a
// stale entry still has a fetchedAt, so a caller using FetchedAt as a
// "do I already have this?" check silently keeps saying yes forever
// after the first fetch. That exact mistake made prewarm (durable.go) a
// no-op after the first 60 seconds of a process's life, which put the
// per-event N+1 it exists to prevent straight back into every request.
func (c *Cache[T]) Fresh(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return ok && time.Since(e.fetchedAt) < e.ttl
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
