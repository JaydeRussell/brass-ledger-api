package bcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeDurableCache is a plain in-memory stand-in for bcp.DurableCache —
// good enough to test this package's *decision logic* (persist only
// once-immutable data, trust anything durably cached unconditionally on
// read) without a real Postgres. internal/bcpcache.Store itself isn't
// unit tested for the same reason internal/user/store.go isn't: it's a
// thin marshal/exec wrapper that needs a real database to test
// meaningfully.
type fakeDurableCache struct {
	data map[string][]byte
	sets int
}

func newFakeDurableCache() *fakeDurableCache {
	return &fakeDurableCache{data: make(map[string][]byte)}
}

func (f *fakeDurableCache) Get(_ context.Context, key string, dest any) (bool, error) {
	raw, ok := f.data[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, dest)
}

func (f *fakeDurableCache) Set(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f.data[key] = raw
	f.sets++
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
