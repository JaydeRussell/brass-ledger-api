package bcp

import (
	"context"
	"encoding/json"
	"time"
)

// DurableCache is the persistence layer a Client optionally writes
// permanently-cacheable BCP responses to and reads them back from —
// implemented against Postgres by internal/bcpcache (see
// internal/db/migrations/0004_bcp_durable_cache.sql), kept as a small
// interface here so this package doesn't need to depend on pgx directly.
//
// A nil DurableCache (the default — NewClient/NewClientWithBaseURL don't
// set one) just means every fetch behaves exactly as it always has: real
// BCP calls, deduped/rate-limited only by the in-memory Cache in
// cache.go. Every fetch* function across this package that supports
// durable caching (events.go, players.go, pairings.go, placings.go,
// itc.go) checks for nil before touching it, so this is entirely
// optional and every existing test (which never sets one) is unaffected.
//
// Get decodes the stored JSON into dest (a pointer) and reports whether
// a value was found at all — false both when nothing is stored under
// key and when something is but under a different version than the one
// passed in (see CacheSchemaVersion below). Set marshals value as JSON
// and stores it, tagged with version — callers only ever call Set once
// they've confirmed the underlying BCP data can never change again (an
// already-concluded event's info, roster, pairings, placings; a
// league's gw_itc/hobby classification, which is effectively
// permanent). Because of that, a version match is trusted
// unconditionally on read — there's no TTL or "is this still valid"
// check beyond the version, unlike the in-memory Cache.
//
// GetFresh, GetMany and Delete exist for the two kinds of durable value
// that *aren't* permanent:
//
//   - GetFresh is Get plus a maximum age, for data that does change —
//     a player's own event/placing history (history.go), which gains
//     entries whenever they register for something. It also returns when
//     the row was written, so an entry seeded from it can carry its real
//     age rather than claiming it was just fetched. The cached_at column
//     has existed since migration 0004 and this is the first thing to
//     read it back.
//   - GetMany reads a whole set of keys in one query, each with the time
//     it was written — callers holding values that expire need the age
//     to judge them, not just the bytes. Callers that know
//     their key set up front (see Client.PrewarmEventInfo) use it to
//     avoid one round trip per key — with a cold in-memory cache, a
//     player's stats page was doing one SELECT per event they'd ever
//     attended, sequentially.
//   - Delete drops a row so a user-initiated refresh isn't immediately
//     undone by reloading the same stale value from Postgres.
//
// DurableRow is one stored value and when it was written. The timestamp
// matters because not everything in this cache is permanent: an event
// that hasn't started yet is stored with a lifetime (see eventInfoTTL),
// so a reader has to know how old the row is before trusting it.
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
// previously-written row (including rows written before this version
// column even existed, which the 0012 migration backfilled to 0) then
// simply stops matching on its next read and gets transparently
// refetched from BCP and re-cached at the new version. This is what
// caught PlacingEntry's Faction/SubFaction fields (added in commit
// b6b26da) being permanently absent from any event durably cached
// before that change shipped — see that incident's write-up before
// assuming a durable row is safe to read as-is after any schema change.
const CacheSchemaVersion = 1

// SetDurableCache installs c's durable cache. Call once, right after
// NewClient/NewClientWithBaseURL, before any fetches happen — it's not
// safe to swap concurrently with in-flight requests.
func (c *Client) SetDurableCache(d DurableCache) {
	c.durable = d
}

// prewarm loads every key that's in the durable cache but not yet in
// memory, in one query, and seeds the in-memory cache with what it
// finds. Keys already in memory are skipped (a live entry is never worse
// than a stored one), and keys with no row are simply left alone for the
// normal per-key fetch path to handle.
//
// Seeded entries carry the row's own cached_at, and the decode function
// is free to reject a row that's too old for what it holds — event info
// is no longer all-permanent, since an event that hasn't started yet is
// now stored with a lifetime rather than not stored at all.
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
// is most page loads) that was one sequential Postgres round trip per
// event the player had ever attended.
//
// Only ended events are ever written durably (see events.go), so
// anything this finds is by definition an event whose info can't change.
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
