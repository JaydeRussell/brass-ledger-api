package bcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- shared test helpers -------------------------------------------------
//
// Used across this package's other _test.go files (events_test.go,
// players_test.go, pairings_test.go, placings_test.go, itc_test.go,
// history_test.go) — kept here since none of those files is more
// entitled to "own" them than the others.

// newTestClient builds a Client whose v1/v2/site base URLs all point at
// server — letting these tests exercise the real fetch/parse logic
// without ever reaching the actual BCP API. Thin wrapper over the
// exported NewClientWithBaseURL, which internal/api's tests also use.
func newTestClient(server *httptest.Server) *Client {
	return NewClientWithBaseURL(server.URL)
}

// jsonHandler replies with a fixed status and JSON body regardless of the
// request, which is all these table-driven cases need — each one only
// cares about how the Client parses a given upstream response.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func eventInfoEqual(a, b EventInfo) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func itcRankingEqual(a, b *ItcRanking) bool {
	if a == nil || b == nil {
		return a == b
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// TestPairingsCacheDoesNotServeStale guards the one exclusion by name,
// at the level someone would actually change it.
//
// The cache-level test in cache_test.go proves the mechanism is opt-in;
// this proves the opt-in wasn't applied here. They are different
// mistakes: enabling it on pairings is a one-word edit in client.go
// that no cache-level test would notice.
func TestPairingsCacheDoesNotServeStale(t *testing.T) {
	c := NewClient()

	if c.pairings.serveStale {
		t.Error("the pairings cache serves stale entries. The moment a round's pairings publish " +
			"is the one moment in this app where being a minute behind is something a person " +
			"standing at a venue notices — see NewCacheWithStaleWhileRevalidate's doc comment.")
	}
	// Its neighbours are meant to, so a blanket removal is caught too.
	if !c.eventInfo.serveStale {
		t.Error("the event-info cache stopped serving stale entries")
	}
	if !c.placings.serveStale {
		t.Error("the placings cache stopped serving stale entries")
	}
}

func TestRedactUserIDs(t *testing.T) {
	cases := map[string]string{
		"https://x/v1/players?userId=abc123&limit=100":            "https://x/v1/players?userId=redacted&limit=100",
		"https://x/v1/placings?leagueId=L1&userId[]=abc123":       "https://x/v1/placings?leagueId=L1&userId[]=redacted",
		"https://x/v1/eventplacings?userId%5B%5D=abc&nextKey=k":   "https://x/v1/eventplacings?userId%5B%5D=redacted&nextKey=redacted",
		"https://x/v2/events/evt-1?role=true":                     "https://x/v2/events/evt-1?role=true",
		"https://x/v1/events?gameSystemId=G&searchString=my+club": "https://x/v1/events?gameSystemId=G&searchString=redacted",
	}
	for in, want := range cases {
		if got := redactUserIDs(in); got != want {
			t.Errorf("redactUserIDs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDistanceFrom(t *testing.T) {
	denver := &EventSearchNear{Lat: 39.74, Lon: -104.99, RadiusMiles: 50}
	// Westminster, CO, about 11 miles north-west of downtown Denver.
	if got := distanceFrom(denver, []float64{-105.0745677, 39.8838055}); got == nil || *got != 11 {
		t.Errorf("Denver to Westminster = %v, want 11 miles", got)
	}
	// Denver to London is about 4,690 miles.
	if got := distanceFrom(denver, []float64{-0.1276, 51.5072}); got == nil || *got < 4650 || *got > 4730 {
		t.Errorf("Denver to London = %v, want about 4690 miles", got)
	}
	if distanceFrom(nil, []float64{1, 2}) != nil || distanceFrom(denver, nil) != nil {
		t.Error("want nil without a search centre or a venue location")
	}
}
