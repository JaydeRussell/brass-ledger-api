package bcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPairingsKey_RoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		eventID     string
		pairingType string
		round       int
	}{
		{"individual round 1", "evt-1", "Pairing", 1},
		{"team round 5", "evt-2", "TeamPairing", 5},
		{"event id containing no colons", "abc123", "Pairing", 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := pairingsKey(tc.eventID, tc.pairingType, tc.round)
			gotEventID, gotType, gotRound, err := splitPairingsKey(key)
			if err != nil {
				t.Fatalf("splitPairingsKey(%q) returned error: %v", key, err)
			}
			if gotEventID != tc.eventID || gotType != tc.pairingType || gotRound != tc.round {
				t.Errorf("round-trip mismatch for %q: got (%q, %q, %d), want (%q, %q, %d)",
					key, gotEventID, gotType, gotRound, tc.eventID, tc.pairingType, tc.round)
			}
		})
	}
}

func TestSplitPairingsKey_Errors(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"missing parts", "eventonly"},
		{"only two parts", "event:Pairing"},
		{"non-numeric round", "event:Pairing:notanumber"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := splitPairingsKey(tc.key); err == nil {
				t.Errorf("splitPairingsKey(%q) succeeded, want an error", tc.key)
			}
		})
	}
}

func TestFetchRoundPairings(t *testing.T) {
	body := `{"active": [
		{"id": "pair-1", "pairingType": "Pairing", "table": 1, "round": 2, "player1Id": "p1", "player2Id": "p2"}
	]}`
	server := httptest.NewServer(jsonHandler(http.StatusOK, body))
	defer server.Close()
	client := newTestClient(server)

	got, err := client.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 2)
	if err != nil {
		t.Fatalf("FetchRoundPairings returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "pair-1" {
		t.Errorf("FetchRoundPairings = %+v, want a single pairing with id %q", got, "pair-1")
	}
}

// TestInvalidateRoundPairings mirrors events_test.go's
// TestInvalidateEventInfo: this only confirms InvalidateRoundPairings
// reaches the right (event, pairingType, round) cache key — the
// manual-invalidate floor's own behavior (throttled vs. actually
// bypassing the cache once it elapses) is covered centrally by
// cache_test.go's TestCache_Invalidate.
func TestInvalidateRoundPairings(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1/pairings", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"active": []}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)

	if _, err := client.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 2); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := client.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 2); err != nil {
		t.Fatalf("second fetch (should be cached): %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d before invalidating, want 1", calls)
	}

	// A different (pairingType, round) is untouched by invalidating this
	// one triple — confirms InvalidateRoundPairings is scoped to the
	// exact key, not the whole event.
	client.InvalidateRoundPairings("evt-1", "TeamPairing", 2)
	if _, err := client.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 2); err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d after invalidating a different pairingType, want 1 (unrelated key)", calls)
	}

	client.InvalidateRoundPairings("evt-1", "Pairing", 2)
	if _, err := client.FetchRoundPairings(context.Background(), "evt-1", "Pairing", 2); err != nil {
		t.Fatalf("fourth fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d after an immediate Invalidate, want 1 (should have been throttled)", calls)
	}
}
