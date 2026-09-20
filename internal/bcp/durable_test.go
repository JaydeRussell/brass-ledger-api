package bcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDurableCache is a plain in-memory stand-in for bcp.DurableCache —
// good enough to test this package's *decision logic* (persist only
// once-immutable data, trust anything durably cached unconditionally on
// read, bust anything cached under a different CacheSchemaVersion)
// without a real Postgres. internal/bcpcache.Store itself isn't unit
// tested for the same reason internal/user/store.go isn't: it's a thin
// marshal/exec wrapper that needs a real database to test meaningfully.
type fakeDurableCache struct {
	data         map[string]fakeDurableCacheEntry
	sets         int
	deletes      int
	getManyCalls int
}

type fakeDurableCacheEntry struct {
	raw     []byte
	version int
	// Zero means "written just now" — a test that needs an entry to look
	// old sets this explicitly (see the GetFresh cases in history_test.go).
	cachedAt time.Time
}

func newFakeDurableCache() *fakeDurableCache {
	return &fakeDurableCache{data: make(map[string]fakeDurableCacheEntry)}
}

func (f *fakeDurableCache) Get(_ context.Context, key string, version int, dest any) (bool, error) {
	entry, ok := f.data[key]
	if !ok || entry.version != version {
		return false, nil
	}
	return true, json.Unmarshal(entry.raw, dest)
}

func (f *fakeDurableCache) Set(_ context.Context, key string, version int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f.data[key] = fakeDurableCacheEntry{raw: raw, version: version, cachedAt: time.Now()}
	f.sets++
	return nil
}

func (f *fakeDurableCache) GetFresh(_ context.Context, key string, version int, maxAge time.Duration, dest any) (bool, time.Time, error) {
	entry, ok := f.data[key]
	if !ok || entry.version != version {
		return false, time.Time{}, nil
	}
	if maxAge > 0 && time.Since(entry.cachedAt) > maxAge {
		return false, entry.cachedAt, nil
	}
	return true, entry.cachedAt, json.Unmarshal(entry.raw, dest)
}

func (f *fakeDurableCache) GetMany(_ context.Context, keys []string, version int) (map[string]DurableRow, error) {
	f.getManyCalls++
	found := make(map[string]DurableRow, len(keys))
	for _, key := range keys {
		entry, ok := f.data[key]
		if !ok || entry.version != version {
			continue
		}
		at := entry.cachedAt
		if at.IsZero() {
			at = time.Now()
		}
		found[key] = DurableRow{Data: json.RawMessage(entry.raw), CachedAt: at}
	}
	return found, nil
}

func (f *fakeDurableCache) Delete(_ context.Context, key string) error {
	delete(f.data, key)
	f.deletes++
	return nil
}

// failingServer answers every request with a 500 — used as a second
// Client's upstream to prove a durable-cache hit never actually reaches
// BCP at all, not just that it's fast.
func failingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to BCP: %s %s (should have been served from the durable cache)", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchEventInfo_DurableCache(t *testing.T) {
	t.Run("an ended event is persisted and served from durable cache on a fresh Client", func(t *testing.T) {
		fake := newFakeDurableCache()
		mux := http.NewServeMux()
		mux.HandleFunc("/events/evt-1", jsonHandler(http.StatusOK, `{"id": "evt-1", "name": "Ended Event", "status": {"ended": true}}`))
		server := httptest.NewServer(mux)
		defer server.Close()
		client := newTestClient(server)
		client.SetDurableCache(fake)

		if _, err := client.FetchEventInfo(context.Background(), "evt-1"); err != nil {
			t.Fatalf("FetchEventInfo: %v", err)
		}
		if fake.sets != 1 {
			t.Fatalf("durable Set called %d times, want 1", fake.sets)
		}

		// A different Client (fresh in-memory cache), same durable cache,
		// pointed at a server that fails any request — should be served
		// entirely from the durable cache.
		client2 := NewClientWithBaseURL(failingServer(t).URL)
		client2.SetDurableCache(fake)
		got, err := client2.FetchEventInfo(context.Background(), "evt-1")
		if err != nil {
			t.Fatalf("FetchEventInfo (from durable cache): %v", err)
		}
		if got.Name != "Ended Event" {
			t.Errorf("Name = %q, want %q", got.Name, "Ended Event")
		}
	})

	t.Run("an event that hasn't ended yet is never persisted", func(t *testing.T) {
		fake := newFakeDurableCache()
		mux := http.NewServeMux()
		mux.HandleFunc("/events/evt-live", jsonHandler(http.StatusOK, `{"id": "evt-live", "name": "Still Going", "status": {"started": true, "ended": false}}`))
		server := httptest.NewServer(mux)
		defer server.Close()
		client := newTestClient(server)
		client.SetDurableCache(fake)

		if _, err := client.FetchEventInfo(context.Background(), "evt-live"); err != nil {
			t.Fatalf("FetchEventInfo: %v", err)
		}
		if fake.sets != 0 {
			t.Errorf("durable Set called %d times, want 0 (event hasn't ended)", fake.sets)
		}
	})
}

// TestDurableCache_VersionMismatchBustsCache is the regression test for
// the incident CacheSchemaVersion's doc comment describes: a row cached
// under an older schema version (here simulated directly, the same way
// a real row written before the 0012 migration would come back at
// version 0) must never be trusted, even though the key matches —
// fetchEventInfoUncached should treat it as a miss, hit BCP for real,
// and re-cache at the current version.
func TestDurableCache_VersionMismatchBustsCache(t *testing.T) {
	fake := newFakeDurableCache()
	fake.data[eventInfoDurableKey("evt-1")] = fakeDurableCacheEntry{
		raw:     []byte(`{"id": "evt-1", "name": "Stale Pre-Version Data", "ended": true}`),
		version: CacheSchemaVersion - 1,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", jsonHandler(http.StatusOK, `{"id": "evt-1", "name": "Fresh From BCP", "status": {"ended": true}}`))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)
	client.SetDurableCache(fake)

	got, err := client.FetchEventInfo(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("FetchEventInfo: %v", err)
	}
	if got.Name != "Fresh From BCP" {
		t.Errorf("Name = %q, want %q (the stale cached row should have been ignored)", got.Name, "Fresh From BCP")
	}

	// Re-cached at the current version — a fresh Client pointed at a
	// failing server should now be served from the durable cache again,
	// with the up-to-date value.
	client2 := NewClientWithBaseURL(failingServer(t).URL)
	client2.SetDurableCache(fake)
	got2, err := client2.FetchEventInfo(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("FetchEventInfo (from durable cache): %v", err)
	}
	if got2.Name != "Fresh From BCP" {
		t.Errorf("Name = %q, want %q", got2.Name, "Fresh From BCP")
	}
	if fake.data[eventInfoDurableKey("evt-1")].version != CacheSchemaVersion {
		t.Errorf("re-cached row's version = %d, want %d", fake.data[eventInfoDurableKey("evt-1")].version, CacheSchemaVersion)
	}
}

func TestFetchLeagueInfo_DurableCache(t *testing.T) {
	fake := newFakeDurableCache()
	mux := http.NewServeMux()
	mux.HandleFunc("/leagues/league-1", jsonHandler(http.StatusOK, `{"name": "Flagship ITC", "gw_itc": true, "hobby": false}`))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)
	client.SetDurableCache(fake)

	if _, err := client.FetchLeagueInfo(context.Background(), "league-1"); err != nil {
		t.Fatalf("FetchLeagueInfo: %v", err)
	}
	if fake.sets != 1 {
		t.Fatalf("durable Set called %d times, want 1 (leagues persist unconditionally, no ended gate)", fake.sets)
	}

	client2 := NewClientWithBaseURL(failingServer(t).URL)
	client2.SetDurableCache(fake)
	got, err := client2.FetchLeagueInfo(context.Background(), "league-1")
	if err != nil {
		t.Fatalf("FetchLeagueInfo (from durable cache): %v", err)
	}
	if got == nil || !got.GwItc || got.Hobby {
		t.Errorf("got %+v, want the flagship league from the durable cache", got)
	}
}

func TestFetchPlayers_DurableCache(t *testing.T) {
	fake := newFakeDurableCache()
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", jsonHandler(http.StatusOK, `{"id": "evt-1", "name": "Ended Event", "status": {"ended": true}}`))
	mux.HandleFunc("/events/evt-1/players", jsonHandler(http.StatusOK, `{"active": [{"id": "p1", "user": {"firstName": "Anna", "lastName": "Adams"}, "faction": {"name": "Necrons"}, "listId": "l1"}]}`))
	mux.HandleFunc("/events/evt-1/teamplayers", jsonHandler(http.StatusNotFound, `not found`))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)
	client.SetDurableCache(fake)

	if _, err := client.FetchPlayers(context.Background(), "evt-1"); err != nil {
		t.Fatalf("FetchPlayers: %v", err)
	}
	// One Set for the event info (fetched internally to check Ended) and
	// one for the roster itself.
	if fake.sets != 2 {
		t.Fatalf("durable Set called %d times, want 2 (event info + roster)", fake.sets)
	}

	client2 := NewClientWithBaseURL(failingServer(t).URL)
	client2.SetDurableCache(fake)
	got, err := client2.FetchPlayers(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("FetchPlayers (from durable cache): %v", err)
	}
	if len(got) != 1 || got[0].Name != "Anna Adams" {
		t.Errorf("got %+v, want the roster from the durable cache", got)
	}
}

func TestFetchRoundPairings_DurableCache(t *testing.T) {
	fake := newFakeDurableCache()
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", jsonHandler(http.StatusOK, `{"id": "evt-1", "name": "Ended Event", "status": {"ended": true}}`))
	mux.HandleFunc("/events/evt-1/pairings", jsonHandler(http.StatusOK, `{"active": [{"id": "pr1", "pairingType": "Pairing", "table": 1}]}`))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)
	client.SetDurableCache(fake)

	if _, err := client.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 1); err != nil {
		t.Fatalf("FetchRoundPairings: %v", err)
	}

	client2 := NewClientWithBaseURL(failingServer(t).URL)
	client2.SetDurableCache(fake)
	got, err := client2.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 1)
	if err != nil {
		t.Fatalf("FetchRoundPairings (from durable cache): %v", err)
	}
	if len(got) != 1 || got[0].ID != "pr1" {
		t.Errorf("got %+v, want the pairings from the durable cache", got)
	}
}

func TestFetchPlacings_DurableCache(t *testing.T) {
	fake := newFakeDurableCache()
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", jsonHandler(http.StatusOK, `{"id": "evt-1", "name": "Ended Event", "status": {"ended": true}}`))
	mux.HandleFunc("/events/evt-1/players", jsonHandler(http.StatusOK, `{"active": [{"id": "pl1", "user": {"firstName": "Anna", "lastName": "Adams"}, "placing": 1}]}`))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)
	client.SetDurableCache(fake)

	if _, err := client.FetchPlacings(context.Background(), "evt-1", false); err != nil {
		t.Fatalf("FetchPlacings: %v", err)
	}

	client2 := NewClientWithBaseURL(failingServer(t).URL)
	client2.SetDurableCache(fake)
	got, err := client2.FetchPlacings(context.Background(), "evt-1", false)
	if err != nil {
		t.Fatalf("FetchPlacings (from durable cache): %v", err)
	}
	if len(got) != 1 || got[0].Name != "Anna Adams" {
		t.Errorf("got %+v, want the placings from the durable cache", got)
	}
}

// --- Player history (the two age-bounded durable values) ---------------

const historyPlayersBody = `{"data": [{"eventId": "evt-1", "event": {"id": "evt-1", "name": "Cold Open"}}]}`

func TestPlayerEventHistory_DurableCacheSurvivesANewClient(t *testing.T) {
	// The actual point of this cache: the deployed container sleeps after
	// ten minutes, so "a new Client" is what a returning visitor gets.
	fake := newFakeDurableCache()

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(historyPlayersBody))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	first := NewClientWithBaseURL(server.URL)
	first.SetDurableCache(fake)
	got, err := first.FetchPlayerEventHistory(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "evt-1" {
		t.Fatalf("first fetch = %+v, want one evt-1 record", got)
	}
	if calls != 1 {
		t.Fatalf("calls = %d after the first fetch, want 1", calls)
	}

	// A second process (fresh in-memory cache, same Postgres) must not
	// reach BCP at all — its upstream fails every request to prove it.
	second := NewClientWithBaseURL(failingServer(t).URL)
	second.SetDurableCache(fake)
	got, err = second.FetchPlayerEventHistory(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "evt-1" {
		t.Errorf("second fetch = %+v, want the durably cached evt-1 record", got)
	}
}

func TestPlayerEventHistory_DurableEntryOlderThanTTLIsRefetched(t *testing.T) {
	fake := newFakeDurableCache()
	fake.data[playerEventHistoryDurableKey("user-1")] = fakeDurableCacheEntry{
		raw:      []byte(`[{"eventId":"stale-evt","eventName":"Stale"}]`),
		version:  CacheSchemaVersion,
		cachedAt: time.Now().Add(-2 * myEventsRefetchInterval),
	}

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(historyPlayersBody))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(fake)

	got, err := client.FetchPlayerEventHistory(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (a durable row past myEventsRefetchInterval must be refetched)", calls)
	}
	if len(got) != 1 || got[0].EventID != "evt-1" {
		t.Errorf("got = %+v, want the freshly fetched evt-1 record, not the stale one", got)
	}
}

func TestPlayerEventHistory_DurableSeedKeepsItsRealFetchedAt(t *testing.T) {
	// FetchedAt is surfaced to the user as "last updated" (see
	// internal/api/me.go's UpcomingFetchedAt). Seeding from the durable
	// cache must carry the age the data really has, not restart its clock.
	stored := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	fake := newFakeDurableCache()
	fake.data[playerEventHistoryDurableKey("user-1")] = fakeDurableCacheEntry{
		raw:      []byte(`[{"eventId":"evt-1","eventName":"Cold Open"}]`),
		version:  CacheSchemaVersion,
		cachedAt: stored,
	}

	client := NewClientWithBaseURL(failingServer(t).URL)
	client.SetDurableCache(fake)
	if _, err := client.FetchPlayerEventHistory(context.Background(), "user-1"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	fetchedAt, ok := client.PlayerEventHistoryFetchedAt("user-1")
	if !ok {
		t.Fatal("PlayerEventHistoryFetchedAt reported nothing cached after a durable hit")
	}
	if !fetchedAt.Equal(stored) {
		t.Errorf("FetchedAt = %s, want the durable row's own cached_at %s (not time.Now())", fetchedAt, stored)
	}
}

func TestInvalidatePlayerEventHistory_DropsTheDurableRowToo(t *testing.T) {
	// Without this, an explicit refresh would clear the in-memory entry
	// and immediately reload the same stale value from Postgres.
	fake := newFakeDurableCache()
	key := playerEventHistoryDurableKey("user-1")
	fake.data[key] = fakeDurableCacheEntry{
		raw:      []byte(`[{"eventId":"evt-1","eventName":"Cold Open"}]`),
		version:  CacheSchemaVersion,
		cachedAt: time.Now().Add(-time.Hour),
	}

	client := NewClientWithBaseURL(failingServer(t).URL)
	client.SetDurableCache(fake)

	client.InvalidatePlayerEventHistory("user-1")
	if _, still := fake.data[key]; still {
		t.Errorf("durable row survived an invalidate — a refresh would serve the stale value again")
	}
	if fake.deletes != 1 {
		t.Errorf("durable Delete called %d times, want 1", fake.deletes)
	}
}

func TestInvalidatePlayerEventHistory_ThrottledCallLeavesTheDurableRowAlone(t *testing.T) {
	// A throttled invalidate keeps the in-memory entry, so dropping the
	// durable copy would send the *next* read to BCP — defeating the
	// throttle rather than respecting it.
	fake := newFakeDurableCache()

	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(historyPlayersBody))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(fake)
	if _, err := client.FetchPlayerEventHistory(context.Background(), "user-1"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	client.InvalidatePlayerEventHistory("user-1") // immediately after — throttled
	if fake.deletes != 0 {
		t.Errorf("durable Delete called %d times on a throttled invalidate, want 0", fake.deletes)
	}
	if _, still := fake.data[playerEventHistoryDurableKey("user-1")]; !still {
		t.Error("durable row was dropped by a throttled invalidate")
	}
}

func TestPlacingHistory_DurableCacheSurvivesANewClient(t *testing.T) {
	fake := newFakeDurableCache()

	mux := http.NewServeMux()
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"placing": 3, "event": {"id": "evt-9", "name": "Concluded", "eventDate": "2026-01-01"}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	first := NewClientWithBaseURL(server.URL)
	first.SetDurableCache(fake)
	if _, err := first.FetchPlacingHistory(context.Background(), "user-1"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	second := NewClientWithBaseURL(failingServer(t).URL)
	second.SetDurableCache(fake)
	got, err := second.FetchPlacingHistory(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "evt-9" {
		t.Errorf("second fetch = %+v, want the durably cached evt-9 entry", got)
	}
}

// --- Prewarm (the batched durable read) --------------------------------

func TestPrewarmEventInfo_OneQueryForTheWholeSet(t *testing.T) {
	// The N+1 this exists to kill: without prewarming, a cold in-memory
	// cache resolves one durable row per event, sequentially.
	fake := newFakeDurableCache()
	ids := []string{"evt-1", "evt-2", "evt-3"}
	for _, id := range ids {
		fake.data[eventInfoDurableKey(id)] = fakeDurableCacheEntry{
			raw:     []byte(`{"id":"` + id + `","name":"Concluded ` + id + `","ended":true}`),
			version: CacheSchemaVersion,
		}
	}

	// Upstream fails every request: everything here must come from the
	// durable cache or not at all.
	client := NewClientWithBaseURL(failingServer(t).URL)
	client.SetDurableCache(fake)

	client.PrewarmEventInfo(context.Background(), ids)
	if fake.getManyCalls != 1 {
		t.Fatalf("GetMany called %d times for %d ids, want 1", fake.getManyCalls, len(ids))
	}

	for _, id := range ids {
		info, err := client.FetchEventInfo(context.Background(), id)
		if err != nil {
			t.Fatalf("FetchEventInfo(%s) after prewarm: %v", id, err)
		}
		if info.ID != id {
			t.Errorf("FetchEventInfo(%s) = %q, want it served from the prewarmed cache", id, info.ID)
		}
	}
}

func TestPrewarmEventInfo_SkipsWhatIsAlreadyInMemory(t *testing.T) {
	fake := newFakeDurableCache()

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-live", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"id": "evt-live", "name": "Still Going", "status": {"started": true, "ended": false}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(fake)

	// Fetched normally first — an in-flight event, so nothing durable.
	if _, err := client.FetchEventInfo(context.Background(), "evt-live"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	client.PrewarmEventInfo(context.Background(), []string{"evt-live"})
	if fake.getManyCalls != 0 {
		t.Errorf("GetMany called %d times, want 0 (the id was already cached in memory)", fake.getManyCalls)
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (prewarm must not refetch)", calls)
	}
}

func TestPrewarmEventInfo_NoDurableCacheIsANoOp(t *testing.T) {
	// The default for NewClient/NewClientWithBaseURL, and what every
	// other test in this package relies on.
	client := NewClientWithBaseURL(failingServer(t).URL)
	client.PrewarmEventInfo(context.Background(), []string{"evt-1"})
	client.PrewarmLeagueInfo(context.Background(), []string{"league-1"})
}

func TestPrewarmLeagueInfo_ServesTheFlagshipLookupFromOneQuery(t *testing.T) {
	fake := newFakeDurableCache()
	fake.data[leagueInfoDurableKey("league-itc")] = fakeDurableCacheEntry{
		raw:     []byte(`{"id":"league-itc","name":"ITC 2026","gwItc":true,"hobby":false}`),
		version: CacheSchemaVersion,
	}
	fake.data[leagueInfoDurableKey("league-hobby")] = fakeDurableCacheEntry{
		raw:     []byte(`{"id":"league-hobby","name":"Hobby Track","gwItc":true,"hobby":true}`),
		version: CacheSchemaVersion,
	}

	client := NewClientWithBaseURL(failingServer(t).URL)
	client.SetDurableCache(fake)

	client.PrewarmLeagueInfo(context.Background(), []string{"league-itc", "league-hobby", "league-itc"})
	if fake.getManyCalls != 1 {
		t.Fatalf("GetMany called %d times, want 1 (and the duplicate id must not add a key)", fake.getManyCalls)
	}

	itc, err := client.FetchLeagueInfo(context.Background(), "league-itc")
	if err != nil {
		t.Fatalf("FetchLeagueInfo: %v", err)
	}
	if itc == nil || !itc.GwItc || itc.Hobby {
		t.Errorf("league-itc = %+v, want the prewarmed flagship league", itc)
	}
}

func TestPrewarmEventInfo_StillBatchesOnceTheInMemoryEntriesGoStale(t *testing.T) {
	// The regression this exists for: prewarm used to skip any id the
	// in-memory cache had *ever* held, because it asked FetchedAt (which
	// reports presence) instead of Fresh (which reports presence within
	// the TTL). Past the TTL the entries are stale but still present, so
	// prewarm issued no query at all and every id fell through to its own
	// sequential durable read — the exact N+1 prewarm exists to prevent,
	// silently, after the first 60 seconds of a process's life.
	fake := newFakeDurableCache()
	ids := []string{"evt-1", "evt-2", "evt-3"}
	for _, id := range ids {
		fake.data[eventInfoDurableKey(id)] = fakeDurableCacheEntry{
			raw:     []byte(`{"id":"` + id + `","name":"Concluded ` + id + `","ended":true}`),
			version: CacheSchemaVersion,
		}
	}

	client := NewClientWithBaseURL(failingServer(t).URL)
	client.SetDurableCache(fake)

	client.PrewarmEventInfo(context.Background(), ids)
	if fake.getManyCalls != 1 {
		t.Fatalf("GetMany called %d times on the first prewarm, want 1", fake.getManyCalls)
	}

	// Age every seeded entry past its own TTL, as the clock would. These
	// fixtures are ended events, which eventInfoTTL keeps for
	// eventInfoEndedTTL rather than the default minute — so the offset
	// has to clear that, not the short default.
	for _, id := range ids {
		at, ok := client.eventInfo.FetchedAt(id)
		if !ok {
			t.Fatalf("expected %s to be seeded in memory after prewarm", id)
		}
		client.eventInfo.Put(id, mustEventInfo(t, client, id), at.Add(-2*eventInfoEndedTTL))
	}

	client.PrewarmEventInfo(context.Background(), ids)
	if fake.getManyCalls != 2 {
		t.Errorf("GetMany called %d times total, want 2 — stale entries must be re-batched, not skipped", fake.getManyCalls)
	}
}

func mustEventInfo(t *testing.T, c *Client, id string) EventInfo {
	t.Helper()
	info, err := c.FetchEventInfo(context.Background(), id)
	if err != nil {
		t.Fatalf("FetchEventInfo(%s): %v", id, err)
	}
	return info
}

func TestCacheFresh_DistinguishesStaleFromAbsent(t *testing.T) {
	calls := 0
	c := NewCacheWithTTL(func(_ context.Context, key string) (string, error) {
		calls++
		return "v", nil
	}, minRefetchInterval)

	if c.Fresh("k") {
		t.Error("Fresh reported true for a key that was never fetched")
	}
	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !c.Fresh("k") {
		t.Error("Fresh reported false immediately after a successful fetch")
	}

	// Age it past the TTL: still present (FetchedAt says so), no longer
	// fresh (Get would refetch).
	at, _ := c.FetchedAt("k")
	c.Put("k", "v", at.Add(-2*minRefetchInterval))
	if _, present := c.FetchedAt("k"); !present {
		t.Error("FetchedAt should still report a stale entry as present")
	}
	if c.Fresh("k") {
		t.Error("Fresh reported true for an entry past its TTL — this is the bug that made prewarm a no-op")
	}
}

// --- Upcoming events now survive a restart -----------------------------

func farOffEventBody(id string) string {
	start := time.Now().Add(60 * 24 * time.Hour).Format(time.RFC3339)
	return `{"id": "` + id + `", "name": "Two Months Away", ` +
		`"status": {"started": false, "ended": false}, "dates": {"start": "` + start + `"}}`
}

// The production case this exists for: an account whose only uncached
// lookup was a single event two months out. Its in-memory TTL is six
// hours, but the container sleeps after ten minutes — so before this,
// every cold process re-fetched an event that wasn't happening for two
// months, forever, because the only cache holding it never lived long
// enough to be used.
func TestEventInfo_FarOffEventSurvivesANewClient(t *testing.T) {
	fake := newFakeDurableCache()

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-future", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(farOffEventBody("evt-future")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	first := NewClientWithBaseURL(server.URL)
	first.SetDurableCache(fake)
	if _, err := first.FetchEventInfo(context.Background(), "evt-future"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d after the first fetch, want 1", calls)
	}
	if _, stored := fake.data[eventInfoDurableKey("evt-future")]; !stored {
		t.Fatal("a far-off event was not persisted — its six-hour TTL is useless in a ten-minute process")
	}

	// A new process, same Postgres. Its upstream fails every request, so
	// anything it returns came from the durable cache.
	second := NewClientWithBaseURL(failingServer(t).URL)
	second.SetDurableCache(fake)
	got, err := second.FetchEventInfo(context.Background(), "evt-future")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got.ID != "evt-future" || got.Started || got.Ended {
		t.Errorf("second fetch = %+v, want the durably cached upcoming event", got)
	}
}

func TestEventInfo_FarOffEventIsRefetchedOnceItsTTLLapses(t *testing.T) {
	fake := newFakeDurableCache()
	fake.data[eventInfoDurableKey("evt-future")] = fakeDurableCacheEntry{
		raw:      []byte(farOffEventBody("evt-future")),
		version:  CacheSchemaVersion,
		cachedAt: time.Now().Add(-2 * eventInfoFarOffTTL),
	}

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-future", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(farOffEventBody("evt-future")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(fake)
	if _, err := client.FetchEventInfo(context.Background(), "evt-future"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 — a stored upcoming event past eventInfoFarOffTTL must be refetched, "+
			"not trusted indefinitely the way an ended one is", calls)
	}
}

// The other half of the rule: an event being played right now must NOT
// be persisted. Its round and standings move, and a Postgres write per
// request would buy nothing.
func TestEventInfo_InProgressEventIsNotPersisted(t *testing.T) {
	fake := newFakeDurableCache()

	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-live", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-live", "name": "Happening Now", "status": {"started": true, "ended": false}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(fake)
	if _, err := client.FetchEventInfo(context.Background(), "evt-live"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, stored := fake.data[eventInfoDurableKey("evt-live")]; stored {
		t.Error("an in-progress event was persisted — its current round changes, so a stored copy goes stale immediately")
	}
}
