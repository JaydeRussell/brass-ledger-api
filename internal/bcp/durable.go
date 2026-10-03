package bcp

import (
	"context"
	"encoding/json"
	"time"
)

// DurableCache is the persistence layer a Client optionally writes BCP
// responses to and reads them back from — implemented against Postgres
// by internal/bcpcache, kept as a small interface here so this package
// doesn't need to depend on pgx directly.
//
// A nil DurableCache (the default — NewClient/NewClientWithBaseURL don't
// set one) means every fetch goes to BCP, deduped and rate-limited only
// by the in-memory Cache in cache.go. Every fetch* function in this
// package that supports durable caching (events.go, players.go,
// pairings.go, placings.go, itc.go, history.go) checks for nil before
// touching it, so tests that never set one are unaffected.
//
// Set marshals value as JSON and stores it, tagged with version. Rows
// come in two kinds:
//
//   - Permanent: data that can't change again — a concluded event's
//     roster, pairings and placings, and a league's gw_itc/hobby
//     classification. These are read with Get, which decodes the stored
//     JSON into dest (a pointer) and reports whether a row was found
//     under key at the current version (see CacheSchemaVersion). A
//     version match is trusted with no age check.
//   - With a lifetime: a player's event and placing history
//     (history.go), ITC rankings (itc.go), and event info for an event
//     that hasn't concluded yet (events.go, see eventInfoTTL). These are
//     read with GetFresh or GetMany, which return when the row was
//     written so the caller can judge its age.
//
// GetFresh is Get plus a maximum age (0 means no bound, for callers that
// apply their own rule to the returned timestamp). The timestamp also
// lets an entry seeded from the row carry its real age rather than
// claiming it was just fetched.
//
// GetMany reads a whole set of keys in one query, each with the time it
// was written. Callers that know their key set up front (see
// Client.PrewarmEventInfo) use it to avoid one sequential round trip per
// key when the in-memory cache is cold.
//
// Delete drops a row so a user-initiated refresh isn't immediately
// undone by reloading the same stale value from Postgres.
//
// DurableRow is one stored value and when it was written.
type DurableRow struct {
	Data     json.RawMessage
	CachedAt time.Time
}

type DurableCache interface {
	Get(ctx context.Context, key string, version int, dest any) (bool, error)
	Set(ctx context.Context, key string, version int, value any) error
	GetFresh(ctx context.Context, key string, version int, maxAge time.Duration, dest any) (bool, time.Time, error)
	GetMany(ctx context.Context, keys []string, version int) (map[string]DurableRow, error)
	Delete(ctx context.Context, key string) error
}

// CacheSchemaVersion tags every durable Get/Set call in this package.
// Bump it whenever a struct that flows into the durable cache
// (EventInfo, Player, PairingRecord, PlacingEntry, LeagueInfo,
// PlayerEventRecord, PlacingHistoryEntry) gains or
// changes a field that existing callers should stop trusting — every
// row written at an older version then stops matching on its next read
// and is refetched from BCP and re-cached at the new version. Without a
// bump, a permanent row keeps whatever shape it was written with
// forever, so a new field would stay empty on it.
const CacheSchemaVersion = 1

// SetDurableCache installs c's durable cache. Call once, right after
// NewClient/NewClientWithBaseURL, before any fetches happen — it's not
// safe to swap concurrently with in-flight requests.
func (c *Client) SetDurableCache(d DurableCache) {
	c.durable = d
}

// durableWriteTimeout bounds one background write. Generous relative to
// one Neon round trip (DurableReadCost); this exists so a wedged
// connection can't hold a goroutine open indefinitely, not to enforce
// anything.
const durableWriteTimeout = 15 * time.Second

// maxConcurrentDurableWrites bounds how many background writes are in
// flight at once, so a burst of cache misses can't take over the shared
// pgx pool that request handlers also draw from.
const maxConcurrentDurableWrites = 4

// storeDurably writes value under key without making the caller wait.
//
// Keeping the write off the critical path means a cold fetch answers
// after the BCP round trip alone, which matters for /api/me/events
// fetching every pending event at once. The response doesn't depend on
// the write — the value is already in hand and already being returned.
//
// Losing one costs exactly one future BCP request, the same as when the
// write isn't attempted at all (see eventEndedWithoutFetching). That is
// the whole risk, and it is why this can be fire-and-forget while a
// write the user's own data depended on could not be.
//
// Detached from the request context on purpose — the caller's request
// completing is the normal case, not a reason to abandon the write —
// but bounded, both in concurrency and in time.
func (c *Client) storeDurably(ctx context.Context, key string, value any) {
	if c.durable == nil {
		return
	}
	detached := context.WithoutCancel(ctx)
	c.durableWrites.Add(1)
	go func() {
		defer c.durableWrites.Done()
		c.durableWriteSlots <- struct{}{}
		defer func() { <-c.durableWriteSlots }()

		writeCtx, cancel := context.WithTimeout(detached, durableWriteTimeout)
		defer cancel()
		_ = c.durable.Set(writeCtx, key, CacheSchemaVersion, value)
	}()
}

// FlushDurableWrites blocks until every background write started so far
// has finished.
//
// Two callers, both legitimate: a graceful shutdown, so a deploy
// doesn't throw away work already done; and tests, which would
// otherwise race the very writes they are asserting on.
func (c *Client) FlushDurableWrites() {
	c.durableWrites.Wait()
}

// prewarm loads every key that's in the durable cache but not yet in
// memory, in one query, and seeds the in-memory cache with what it
// finds. Keys already in memory are skipped (a live entry is never worse
// than a stored one), and keys with no row are simply left alone for the
// normal per-key fetch path to handle.
//
// Seeded entries carry the row's own cached_at, and the decode function
// is free to reject a row that's too old for what it holds, such as
// event info for an event that hasn't concluded yet, which is stored
// with a lifetime.
func prewarm[T any](
	ctx context.Context,
	d DurableCache,
	cache *Cache[T],
	ids []string,
	durableKey func(string) string,
	decode func(DurableRow) (T, bool),
) {
	if d == nil || len(ids) == 0 {
		return
	}

	keys := make([]string, 0, len(ids))
	keyToID := make(map[string]string, len(ids))
	for _, id := range ids {
		// Fresh, not FetchedAt: a stale-but-present entry is exactly
		// what this needs to reload, and FetchedAt says yes to those.
		if cache.Fresh(id) {
			continue
		}
		key := durableKey(id)
		if _, dup := keyToID[key]; dup {
			continue
		}
		keys = append(keys, key)
		keyToID[key] = id
	}
	if len(keys) == 0 {
		return
	}

	found, err := d.GetMany(ctx, keys, CacheSchemaVersion)
	if err != nil {
		// Best-effort, like every other durable read: falling back to
		// the per-key path is exactly the old behavior.
		return
	}
	for key, row := range found {
		value, ok := decode(row)
		if !ok {
			continue
		}
		cache.Put(keyToID[key], value, row.CachedAt)
	}
}

// PrewarmEventInfo loads any of these events' durably-cached info into
// memory in a single query.
//
// Worth calling before any loop that fetches event info one id at a
// time. internal/api/stats.go's eventInfoByID does exactly that, once
// per distinct event in a player's whole placing history — with a cold
// in-memory cache (which, given the container sleeps after ten minutes,
// is most page loads) each would otherwise be its own sequential
// Postgres round trip.
//
// Ended events and far-off upcoming ones are written durably (see
// events.go's use of eventInfoTTL); a row for an event that hasn't
// ended is only used while it's younger than eventInfoTTL.
func (c *Client) PrewarmEventInfo(ctx context.Context, eventIDs []string) {
	prewarm(ctx, c.durable, c.eventInfo, eventIDs, eventInfoDurableKey, func(row DurableRow) (EventInfo, bool) {
		var info EventInfo
		if err := json.Unmarshal(row.Data, &info); err != nil {
			return EventInfo{}, false
		}
		// Same rule the in-memory cache and the per-key read use: an
		// ended event is permanent, anything else is only good for as
		// long as eventInfoTTL says.
		if !info.Ended && time.Since(row.CachedAt) >= eventInfoTTL(info) {
			return EventInfo{}, false
		}
		return info, true
	})
}

// PrewarmLeagueInfo is PrewarmEventInfo's counterpart for ITC league
// metadata — see internal/api/stats.go's canonicalPlacingPerEvent, which
// resolves one league per placing to decide which of a player's results
// at an event is the flagship one.
func (c *Client) PrewarmLeagueInfo(ctx context.Context, leagueIDs []string) {
	prewarm(ctx, c.durable, c.leagueInfo, leagueIDs, leagueInfoDurableKey, func(row DurableRow) (*LeagueInfo, bool) {
		var info LeagueInfo
		if err := json.Unmarshal(row.Data, &info); err != nil {
			return nil, false
		}
		// A league's gw_itc/hobby classification is permanent.
		return &info, true
	})
}
