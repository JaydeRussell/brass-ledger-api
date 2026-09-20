package bcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestItcRankingKey_RoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		leagueID  string
		bcpUserID string
	}{
		{"typical ids", "league-2026", "user-42"},
		{"empty user id", "league-2026", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := itcRankingKey(tc.leagueID, tc.bcpUserID)
			gotLeagueID, gotUserID, err := splitItcRankingKey(key)
			if err != nil {
				t.Fatalf("splitItcRankingKey(%q) returned error: %v", key, err)
			}
			if gotLeagueID != tc.leagueID || gotUserID != tc.bcpUserID {
				t.Errorf("round-trip mismatch for %q: got (%q, %q), want (%q, %q)",
					key, gotLeagueID, gotUserID, tc.leagueID, tc.bcpUserID)
			}
		})
	}
}

func TestSplitItcRankingKey_Errors(t *testing.T) {
	if _, _, err := splitItcRankingKey("no-colon-here"); err == nil {
		t.Error("splitItcRankingKey(no colon) succeeded, want an error")
	}
}

// TestFetchCurrentItcLeagueIDForEvent covers the event-anchored
// resolution that replaced a game-system-wide leagues search (see
// itc.go's doc comment for why that search stopped being reliable
// against BCP's real API) — this looks up each of an event's own known
// league ids individually via FetchLeagueInfo (/v1/leagues/:id), which
// still correctly returns gw_itc even though BCP's list endpoint
// (/v1/leagues) no longer does.
func TestFetchCurrentItcLeagueIDForEvent(t *testing.T) {
	cases := []struct {
		name      string
		leagueIDs []string
		infoByID  map[string]string // leagueID -> /v1/leagues/:id response body
		want      string
	}{
		{
			name:      "picks the flagship (gw_itc, non-hobby) league among the event's own leagues",
			leagueIDs: []string{"hobby-league", "flagship-league"},
			infoByID: map[string]string{
				"hobby-league":    `{"name": "Hobby Track", "gw_itc": true, "hobby": true}`,
				"flagship-league": `{"name": "Warhammer Global Rankings 2026", "gw_itc": true, "hobby": false}`,
			},
			want: "flagship-league",
		},
		{
			name:      "no matching league resolves to empty, not an error",
			leagueIDs: []string{"local-rtt-league"},
			infoByID: map[string]string{
				"local-rtt-league": `{"name": "Some Local RTT", "gw_itc": false, "hobby": false}`,
			},
			want: "",
		},
		{
			name:      "a failed lookup on one league doesn't hide a working one",
			leagueIDs: []string{"broken-league", "flagship-league"},
			infoByID: map[string]string{
				"flagship-league": `{"name": "Warhammer Global Rankings 2026", "gw_itc": true, "hobby": false}`,
				// "broken-league" deliberately has no handler entry, so
				// the mux 404s it.
			},
			want: "flagship-league",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			for id, body := range tc.infoByID {
				mux.HandleFunc("/leagues/"+id, jsonHandler(http.StatusOK, body))
			}
			server := httptest.NewServer(mux)
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchCurrentItcLeagueIDForEvent(context.Background(), tc.leagueIDs)
			if err != nil {
				t.Fatalf("FetchCurrentItcLeagueIDForEvent returned error: %v", err)
			}
			if got != tc.want {
				t.Errorf("FetchCurrentItcLeagueIDForEvent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchLeagueInfo(t *testing.T) {
	body := `{"name": "Warhammer Global Rankings 2026", "gw_itc": true, "hobby": false}`
	server := httptest.NewServer(jsonHandler(http.StatusOK, body))
	defer server.Close()
	client := newTestClient(server)

	got, err := client.FetchLeagueInfo(context.Background(), "league-1")
	if err != nil {
		t.Fatalf("FetchLeagueInfo returned error: %v", err)
	}
	want := &LeagueInfo{Name: "Warhammer Global Rankings 2026", GwItc: true, Hobby: false}
	if got == nil || *got != *want {
		t.Errorf("FetchLeagueInfo = %+v, want %+v", got, want)
	}
}

func TestFetchItcRanking(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *ItcRanking
	}{
		{
			name: "a matched user returns their ranking",
			body: `{"data": [{"userId": "u1", "ITCPoints": 1234.5, "placing": 7, "wins": 5, "losses": 2, "ties": 1}]}`,
			want: &ItcRanking{Points: 1234.5, Placing: new(7), Wins: new(5), Losses: new(2), Ties: new(1)},
		},
		{
			name: "an unmatched user comes back as one empty object, not an array/error — resolves to nil",
			body: `{"data": [{}]}`,
			want: nil,
		},
		{
			name: "an empty data array also resolves to nil",
			body: `{"data": []}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(jsonHandler(http.StatusOK, tc.body))
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchItcRanking(context.Background(), "league-1", "u1")
			if err != nil {
				t.Fatalf("FetchItcRanking returned error: %v", err)
			}
			if !itcRankingEqual(got, tc.want) {
				t.Errorf("FetchItcRanking = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// itcStub serves one /placings response and counts how often it's asked.
func itcStub(t *testing.T, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/placings", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &calls
}

// TestFetchItcRanking_SurvivesAColdProcess is the point of giving ITC
// rankings a durable cache.
//
// Every other cached BCP call already had one; this was the only one
// that didn't, and it is the most numerous call the app makes — one per
// player on a roster, so a team event resolves a whole roster's worth.
// The in-memory cache alone was nearly worthless for it, because the
// container sleeps after ten minutes idle, so almost every visit
// started cold and paid for the entire roster again.
//
// Two clients sharing one durable cache is how a cold process is
// modelled: the second has an empty in-memory cache and must still not
// reach BCP.
func TestFetchItcRanking_SurvivesAColdProcess(t *testing.T) {
	server, calls := itcStub(t, `{"data": [{"userId": "u1", "ITCPoints": 1234.5, "placing": 7}]}`)
	durable := newFakeDurableCache()

	first := NewClientWithBaseURL(server.URL)
	first.SetDurableCache(durable)
	got, err := first.FetchItcRanking(context.Background(), "league-1", "u1")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if got == nil || got.Points != 1234.5 {
		t.Fatalf("first fetch returned %+v, want 1234.5 points", got)
	}
	if *calls != 1 {
		t.Fatalf("first fetch made %d upstream calls, want 1", *calls)
	}

	second := NewClientWithBaseURL(server.URL)
	second.SetDurableCache(durable)
	got, err = second.FetchItcRanking(context.Background(), "league-1", "u1")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got == nil || got.Points != 1234.5 {
		t.Errorf("a cold process read %+v from the durable cache, want 1234.5 points", got)
	}
	if *calls != 1 {
		t.Errorf("a cold process made %d upstream calls in total, want 1 — "+
			"the durable cache is what stops every restart re-fetching a whole roster", *calls)
	}
}

// TestFetchItcRanking_NoRankingIsCachedToo covers the answer that isn't
// a value.
//
// "This player has no ranking in this league" is an entirely normal
// result — anyone who hasn't scored in the current season yet — and BCP
// returns it as a 200 with an empty or unmatched record, not an error.
// So goneTTL doesn't apply, and before this the answer had nowhere to
// live: every such player cost a real BCP request on every page load,
// forever, while the page looked perfectly correct. That is the same
// shape as the deleted-event bug in v0.19.7, minus the failure.
func TestFetchItcRanking_NoRankingIsCachedToo(t *testing.T) {
	// One empty object is what BCP actually returns for an unmatched
	// userId — not an empty array, not a 404.
	server, calls := itcStub(t, `{"data": [{}]}`)
	durable := newFakeDurableCache()

	for i := range 3 {
		client := NewClientWithBaseURL(server.URL)
		client.SetDurableCache(durable)
		got, err := client.FetchItcRanking(context.Background(), "league-1", "unranked")
		if err != nil {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
		if got != nil {
			t.Fatalf("fetch %d returned %+v, want nil for a player with no ranking", i+1, got)
		}
	}

	if *calls != 1 {
		t.Errorf("an unranked player cost %d upstream calls across 3 cold processes, want 1.\n"+
			"A 200 that means 'nothing here' still needs caching, or it is a permanent "+
			"per-request tax that nothing in the response makes visible.", *calls)
	}
}

// TestFetchItcRanking_IsKeptForHours pins the TTL to a literal.
//
// The expiry test below ages its row by itcRankingRefetchInterval, which
// says nothing about the value: it passes just as happily if somebody
// puts this cache back on the 60-second default meant for live pairings
// — and that default is precisely the bug being fixed. The same
// circularity CLAUDE.md warns about for latency budgets, and the second
// time it has bitten in this work.
//
// So: a row written an hour ago must still be served. One hour is a
// deliberately weak claim — the real value is twelve — chosen so this
// test pins "not the live-data default" without having to move every
// time the exact TTL is tuned.
func TestFetchItcRanking_IsKeptForHours(t *testing.T) {
	const mustOutlive = time.Hour
	if itcRankingRefetchInterval <= mustOutlive {
		t.Fatalf("itcRankingRefetchInterval is %s, which is not meaningfully longer than the %s "+
			"this test pins. An ITC ranking is a season aggregate that only moves when an event "+
			"concludes; caching it like live pairings is what made a team event's roster cost a "+
			"full round of BCP requests on nearly every visit.", itcRankingRefetchInterval, mustOutlive)
	}

	server, calls := itcStub(t, `{"data": [{"userId": "u1", "ITCPoints": 500, "placing": 3}]}`)
	durable := newFakeDurableCache()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(durable)
	if _, err := client.FetchItcRanking(context.Background(), "league-1", "u1"); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	key := itcRankingDurableKey("league-1", "u1")
	entry := durable.data[key]
	entry.cachedAt = time.Now().Add(-mustOutlive)
	durable.data[key] = entry

	cold := NewClientWithBaseURL(server.URL)
	cold.SetDurableCache(durable)
	got, err := cold.FetchItcRanking(context.Background(), "league-1", "u1")
	if err != nil {
		t.Fatalf("fetch after %s: %v", mustOutlive, err)
	}
	if got == nil || got.Points != 500 {
		t.Errorf("after %s the stored ranking read back as %+v, want 500 points", mustOutlive, got)
	}
	if *calls != 1 {
		t.Errorf("a ranking stored %s ago cost %d upstream calls, want 1 — the TTL is too short", mustOutlive, *calls)
	}
}

// TestFetchItcRanking_StaleDurableRowIsRefetched is the other side of
// the bargain: rankings do move, so a stored one has to expire.
func TestFetchItcRanking_StaleDurableRowIsRefetched(t *testing.T) {
	server, calls := itcStub(t, `{"data": [{"userId": "u1", "ITCPoints": 2000, "placing": 1}]}`)
	durable := newFakeDurableCache()

	client := NewClientWithBaseURL(server.URL)
	client.SetDurableCache(durable)
	if _, err := client.FetchItcRanking(context.Background(), "league-1", "u1"); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	// Age the stored row past itcRankingRefetchInterval.
	key := itcRankingDurableKey("league-1", "u1")
	entry := durable.data[key]
	entry.cachedAt = time.Now().Add(-itcRankingRefetchInterval - time.Hour)
	durable.data[key] = entry

	cold := NewClientWithBaseURL(server.URL)
	cold.SetDurableCache(durable)
	if _, err := cold.FetchItcRanking(context.Background(), "league-1", "u1"); err != nil {
		t.Fatalf("post-expiry fetch: %v", err)
	}
	if *calls != 2 {
		t.Errorf("made %d upstream calls, want 2 — a ranking older than %s must be rechecked, "+
			"or a player's own new result never shows up", *calls, itcRankingRefetchInterval)
	}
}
