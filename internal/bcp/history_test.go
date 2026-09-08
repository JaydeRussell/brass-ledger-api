package bcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDecodeNextKey(t *testing.T) {
	cases := []struct {
		name string
		raw  string // the exact bytes the "nextKey" field decoded to
		want string
	}{
		{"absent field", "", ""},
		{"explicit null", "null", ""},
		{
			name: "already a JSON string (the common case) passes through unwrapped",
			raw:  `"cursor-abc"`,
			want: "cursor-abc",
		},
		{
			// The real case that broke pagination: BCP sending the raw
			// cursor object instead of a base64-encoded string.
			name: "a raw JSON object gets base64-encoded",
			raw:  `{"type":"query","value":{"key":"BCPPoints","type":"double","value":46.36,"objectId":"evt-a"}}`,
			want: "eyJ0eXBlIjoicXVlcnkiLCJ2YWx1ZSI6eyJrZXkiOiJCQ1BQb2ludHMiLCJ0eXBlIjoiZG91YmxlIiwidmFsdWUiOjQ2LjM2LCJvYmplY3RJZCI6ImV2dC1hIn19",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			got, err := decodeNextKey(raw)
			if err != nil {
				t.Fatalf("decodeNextKey(%s) returned error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("decodeNextKey(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestFetchPlayerEventHistory(t *testing.T) {
	cases := []struct {
		name        string
		responses   []string // one body per expected page, in order
		wantEventID []string
		wantErr     bool
	}{
		{
			name: "single page, no nextKey",
			responses: []string{
				`{"data": [
					{"eventId": "evt-1", "event": {"id": "evt-1", "name": "GemHammer RTT"}, "checkedIn": true},
					{"event": {"id": "evt-2", "name": "Local GT"}, "dropped": true}
				]}`,
			},
			wantEventID: []string{"evt-1", "evt-2"},
		},
		{
			name: "follows nextKey across two pages",
			responses: []string{
				`{"data": [{"event": {"id": "evt-1", "name": "Page One Event"}}], "nextKey": "cursor-abc"}`,
				`{"data": [{"event": {"id": "evt-2", "name": "Page Two Event"}}]}`,
			},
			wantEventID: []string{"evt-1", "evt-2"},
		},
		{
			name: "an event with no id at all is skipped",
			responses: []string{
				`{"data": [
					{"event": {}},
					{"event": {"id": "evt-1", "name": "Real Event"}}
				]}`,
			},
			wantEventID: []string{"evt-1"},
		},
		{
			name:      "non-2xx upstream status is an error",
			responses: []string{`{}`},
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
				if tc.wantErr {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if page >= len(tc.responses) {
					t.Fatalf("requested more pages (%d) than the test provided (%d)", page+1, len(tc.responses))
				}
				body := tc.responses[page]
				page++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchPlayerEventHistory(context.Background(), "bcp-user-1")
			if tc.wantErr {
				if err == nil {
					t.Fatal("FetchPlayerEventHistory succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchPlayerEventHistory returned error: %v", err)
			}
			if len(got) != len(tc.wantEventID) {
				t.Fatalf("FetchPlayerEventHistory returned %d records, want %d (%+v)", len(got), len(tc.wantEventID), got)
			}
			for i, rec := range got {
				if rec.EventID != tc.wantEventID[i] {
					t.Errorf("record %d EventID = %q, want %q", i, rec.EventID, tc.wantEventID[i])
				}
			}
		})
	}
}

func TestPlayerEventHistoryFetchedAt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/players", jsonHandler(http.StatusOK, `{"data": []}`))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)

	if _, ok := client.PlayerEventHistoryFetchedAt("bcp-user-1"); ok {
		t.Error("PlayerEventHistoryFetchedAt before any fetch reported ok=true, want false")
	}

	if _, err := client.FetchPlayerEventHistory(context.Background(), "bcp-user-1"); err != nil {
		t.Fatalf("FetchPlayerEventHistory: %v", err)
	}
	if _, ok := client.PlayerEventHistoryFetchedAt("bcp-user-1"); !ok {
		t.Error("PlayerEventHistoryFetchedAt after a successful fetch reported ok=false, want true")
	}
}

func TestInvalidatePlayerEventHistory(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"data": []}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)

	if _, err := client.FetchPlayerEventHistory(context.Background(), "bcp-user-1"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := client.FetchPlayerEventHistory(context.Background(), "bcp-user-1"); err != nil {
		t.Fatalf("second fetch (should be cached): %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d before invalidating, want 1 (second fetch should have been cached)", calls)
	}

	// Invalidate is throttled to once per minManualInvalidateInterval (see
	// cache.go) — calling it immediately after the fetch above is
	// expected to be a no-op, same as TestCache_Invalidate's own
	// "does nothing if fetched too recently" case. What this test cares
	// about is that InvalidatePlayerEventHistory really does reach the
	// underlying cache's Invalidate for the right key — not re-testing
	// the throttle itself.
	client.InvalidatePlayerEventHistory("bcp-user-1")
	if _, err := client.FetchPlayerEventHistory(context.Background(), "bcp-user-1"); err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d after an immediate Invalidate, want 1 (should have been throttled, not forced a real refetch)", calls)
	}
}

func TestFetchPlacingHistory(t *testing.T) {
	cases := []struct {
		name        string
		responses   []string
		wantEventID []string // expected order after sorting most-recent-first
		wantErr     bool
	}{
		{
			name: "sorts most-recent event first",
			responses: []string{
				`{"data": [
					{"placing": 6, "points": 40.54, "event": {"id": "evt-old", "name": "Old Event", "eventDate": "2021-07-31T16:01:10.358Z"}},
					{"placing": 115, "points": 54.97, "event": {"id": "evt-new", "name": "New Event", "eventDate": "2024-09-21T11:30:00.000Z"}, "team": {"name": "Gem Wargaming"}}
				]}`,
			},
			wantEventID: []string{"evt-new", "evt-old"},
		},
		{
			name: "an empty event object (edge case seen in real data) is skipped",
			responses: []string{
				`{"data": [
					{"placing": 6, "points": 40.54, "event": {}},
					{"placing": 1, "points": 99.9, "event": {"id": "evt-1", "name": "Real Event", "eventDate": "2024-01-01T00:00:00.000Z"}}
				]}`,
			},
			wantEventID: []string{"evt-1"},
		},
		{
			name: "follows nextKey across two pages",
			responses: []string{
				`{"data": [{"placing": 1, "event": {"id": "evt-a", "name": "A", "eventDate": "2024-01-01T00:00:00.000Z"}}], "nextKey": "cursor-xyz"}`,
				`{"data": [{"placing": 2, "event": {"id": "evt-b", "name": "B", "eventDate": "2023-01-01T00:00:00.000Z"}}]}`,
			},
			wantEventID: []string{"evt-a", "evt-b"},
		},
		{
			// Regression test for a real failure: BCP returned nextKey as
			// the raw, unencoded cursor object on this endpoint instead of
			// the usual base64-encoded JSON string — confirmed against an
			// actual response, not a hypothetical. decodeNextKey has to
			// handle this shape too, or pagination breaks with a JSON
			// decode error on page 2.
			name: "a raw (unencoded) nextKey object, as BCP was observed actually sending",
			responses: []string{
				`{"data": [{"placing": 1, "event": {"id": "evt-a", "name": "A", "eventDate": "2024-01-01T00:00:00.000Z"}}], "nextKey": {"type": "query", "value": {"key": "BCPPoints", "type": "double", "value": 46.36, "objectId": "evt-a"}}}`,
				`{"data": [{"placing": 2, "event": {"id": "evt-b", "name": "B", "eventDate": "2023-01-01T00:00:00.000Z"}}]}`,
			},
			wantEventID: []string{"evt-a", "evt-b"},
		},
		{
			name:      "non-2xx upstream status is an error",
			responses: []string{`{}`},
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
				if tc.wantErr {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if page >= len(tc.responses) {
					t.Fatalf("requested more pages (%d) than the test provided (%d)", page+1, len(tc.responses))
				}
				body := tc.responses[page]
				page++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchPlacingHistory(context.Background(), "bcp-user-1")
			if tc.wantErr {
				if err == nil {
					t.Fatal("FetchPlacingHistory succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchPlacingHistory returned error: %v", err)
			}
			if len(got) != len(tc.wantEventID) {
				t.Fatalf("FetchPlacingHistory returned %d entries, want %d (%+v)", len(got), len(tc.wantEventID), got)
			}
			for i, entry := range got {
				if entry.EventID != tc.wantEventID[i] {
					t.Errorf("entry %d EventID = %q, want %q", i, entry.EventID, tc.wantEventID[i])
				}
			}
		})
	}
}

// FetchPlacingHistory's table-driven test above only checks EventID
// ordering, so a regression in decoding any other field (like LeagueID,
// which internal/api/stats.go's canonicalPlacingPerEvent depends on to
// tell a flagship-league placing apart from a Hobby Track one) wouldn't
// be caught there — checked separately here.
func TestFetchPlacingHistory_DecodesLeagueID(t *testing.T) {
	body := `{"data": [
		{"placing": 5, "event": {"id": "evt-a", "name": "A", "eventDate": "2024-01-01T00:00:00.000Z"}, "leagueId": "league-flagship"}
	]}`
	server := httptest.NewServer(jsonHandler(http.StatusOK, body))
	defer server.Close()
	client := newTestClient(server)

	got, err := client.FetchPlacingHistory(context.Background(), "bcp-user-1")
	if err != nil {
		t.Fatalf("FetchPlacingHistory returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].LeagueID != "league-flagship" {
		t.Errorf("LeagueID = %q, want %q", got[0].LeagueID, "league-flagship")
	}
}
