package bcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// TestHistoryCrawlsStopAtTheCapAndSaySo covers the page cap on both
// history feeds — that it holds, and that hitting it is visible.
//
// The cap holding was never tested at all. Worse, hitting it used to be
// entirely silent: an account past maxHistoryPages*100 records simply
// stopped having a full history, with nothing in the logs, the error
// path or the response to say events had been dropped. A wrong-data bug
// that looks exactly like correct data is the hardest kind to notice,
// which is the whole reason this asserts on the log line and not just
// the request count.
func TestHistoryCrawlsStopAtTheCapAndSaySo(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		feed string
		call func(*Client, context.Context) (int, error)
	}{
		{
			name: "registration feed",
			path: "/players",
			body: `{"data": [{"event": {"id": "evt-%d", "name": "Event %d"}}], "nextKey": "cursor-%d"}`,
			feed: "registration",
			call: func(c *Client, ctx context.Context) (int, error) {
				got, err := c.FetchPlayerEventHistory(ctx, "user-endless")
				return len(got), err
			},
		},
		{
			name: "placing feed",
			path: "/eventplacings",
			body: `{"data": [{"placing": 1, "event": {"id": "evt-%d", "name": "Event %d", "eventDate": "2024-01-0%d"}}], "nextKey": "cursor-%d"}`,
			feed: "placing",
			call: func(c *Client, ctx context.Context) (int, error) {
				got, err := c.FetchPlacingHistory(ctx, "user-endless")
				return len(got), err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An upstream that never stops offering another page — the
			// only way to reach the cap deliberately.
			var pages atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc(tc.path, func(w http.ResponseWriter, _ *http.Request) {
				n := int(pages.Add(1))
				_, _ = w.Write(fmt.Appendf(nil, tc.body, n, n, n, n))
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			client := NewClientWithBaseURL(server.URL)
			count, err := tc.call(client, context.Background())
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}

			// Pinned to a literal, not to maxHistoryPages itself.
			// Asserting the crawl made maxHistoryPages requests is
			// circular — it passes for any value of the constant,
			// including one somebody shrinks by accident. The same
			// circularity CLAUDE.md warns about for latency budgets.
			const wantCap = 10
			if maxHistoryPages != wantCap {
				t.Fatalf("maxHistoryPages is %d, this test is written against %d. "+
					"Changing the cap is a real decision about how much of BCP's API one page load may crawl "+
					"and how much history a user silently loses — move this literal and say why in the commit.",
					maxHistoryPages, wantCap)
			}
			if got := int(pages.Load()); got != wantCap {
				t.Errorf("crawled %d pages, want exactly %d — the safety cap is what stops "+
					"one page load becoming an unbounded crawl of BCP's API", got, wantCap)
			}
			if count != wantCap {
				t.Errorf("kept %d records from %d single-record pages, want %d", count, wantCap, wantCap)
			}
			if !strings.Contains(logs.String(), "truncated at") || !strings.Contains(logs.String(), tc.feed) {
				t.Errorf("hitting the cap logged %q; want a %s-feed truncation warning.\n"+
					"Silence here is the actual bug: the response looks correct while events are missing from it.",
					logs.String(), tc.feed)
			}
		})
	}
}

// TestHistoryCrawlThatFinishesSaysNothing is the other half of the cap
// test above: a crawl that runs out of pages naturally must not claim
// it was truncated.
//
// This is the easy half to get wrong. The cursor has to survive the
// loop for the truncation check to see it, and the obvious way to
// arrange that — assigning it before testing for the end — leaves the
// *previous* page's cursor in hand when the feed stops, so every
// complete crawl reports itself truncated. A warning that fires on
// every normal request is worse than no warning: it trains whoever
// reads the logs to skip the line that was supposed to matter.
func TestHistoryCrawlThatFinishesSaysNothing(t *testing.T) {
	pages := []string{
		`{"data": [{"event": {"id": "evt-1", "name": "Page One"}}], "nextKey": "cursor-abc"}`,
		`{"data": [{"event": {"id": "evt-2", "name": "Page Two"}}]}`,
	}
	var n atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(pages) {
			t.Errorf("crawled past the last page (request %d) — the feed said it was done", i+1)
			i = len(pages) - 1
		}
		_, _ = w.Write([]byte(pages[i]))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	got, err := NewClientWithBaseURL(server.URL).FetchPlayerEventHistory(context.Background(), "user-two-pages")
	if err != nil {
		t.Fatalf("FetchPlayerEventHistory: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d records across two pages, want 2", len(got))
	}
	if strings.Contains(logs.String(), "truncated") {
		t.Errorf("a crawl that read every page logged a truncation warning: %q.\n"+
			"The cursor from the second-to-last page is being mistaken for 'there is more'.", logs.String())
	}
}
