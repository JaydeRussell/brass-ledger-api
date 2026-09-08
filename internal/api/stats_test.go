package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

func newStatsTestEcho(store userStore, bcpClient *bcp.Client) *echo.Echo {
	e := echo.New()
	RegisterStatsRoutes(e, store, bcpClient)
	return e
}

func TestPlayerStats_RequiresSignIn(t *testing.T) {
	e := newStatsTestEcho(newFakeUserStore(), bcp.NewClient())
	req := httptest.NewRequest(http.MethodGet, "/api/me/stats", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPlayerStats_NotLinked(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newStatsTestEcho(store, bcp.NewClient())

	req := httptest.NewRequest(http.MethodGet, "/api/me/stats", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp playerStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if resp.Linked {
		t.Error("linked = true, want false for an account with no BCP profile set")
	}
	if len(resp.Factions) != 0 || resp.TotalEvents != 0 {
		t.Errorf("expected an empty stats summary, got %+v", resp)
	}
}

// stubBCPStatsServer serves the BCP endpoints GET /api/me/stats needs:
// the placings-history list (a GT, an RTT, an unclassifiable entry with
// no end date, a *team* event scored under two different leagues at once
// — a flagship-ITC row and a better-looking Hobby Track row, same as BCP
// really does — to prove canonicalPlacingPerEvent picks the flagship one
// rather than whichever happens to be numerically lower, and a *solo*
// RTT whose placing-history row still carries a club/team name even
// though EventInfo.TeamEvent is false — the real bug this was built to
// catch, confirmed against real BCP data: a Team field being non-empty
// does NOT mean the event itself was team-format), the two leagues, and
// event info for every event (needed for both TeamEvent and, for the
// most recent one, a game system id).
func stubBCPStatsServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"placing": 2, "points": 80, "faction": {"name": "World Eaters"}, "event": {"id": "evt-gt", "name": "A GT", "eventDate": "2026-03-01T00:00:00.000Z", "eventEndDate": "2026-03-02T00:00:00.000Z"}},
			{"placing": 10, "points": 40, "faction": {"name": "World Eaters"}, "event": {"id": "evt-rtt", "name": "An RTT", "eventDate": "2026-02-01T00:00:00.000Z", "eventEndDate": "2026-02-01T00:00:00.000Z"}},
			{"placing": 1, "points": 90, "faction": {"name": "Black Legion"}, "event": {"id": "evt-nodate", "name": "No End Date", "eventDate": "2026-01-01T00:00:00.000Z"}},
			{"placing": 5, "points": 60, "faction": {"name": "World Eaters"}, "team": {"name": "The Skulltakers"}, "leagueId": "league-flagship", "event": {"id": "evt-team", "name": "A Team GT", "eventDate": "2025-12-01T00:00:00.000Z", "eventEndDate": "2025-12-03T00:00:00.000Z"}},
			{"placing": 2, "points": 0, "faction": {"name": "World Eaters"}, "team": {"name": "The Skulltakers"}, "leagueId": "league-hobby", "event": {"id": "evt-team", "name": "A Team GT", "eventDate": "2025-12-01T00:00:00.000Z", "eventEndDate": "2025-12-03T00:00:00.000Z"}},
			{"placing": 3, "points": 55, "faction": {"name": "World Eaters"}, "team": {"name": "The Skulltakers"}, "event": {"id": "evt-club-solo", "name": "A Solo RTT With A Club Tag", "eventDate": "2025-11-01T00:00:00.000Z", "eventEndDate": "2025-11-01T00:00:00.000Z"}}
		]}`))
	})
	mux.HandleFunc("/leagues/league-flagship", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Flagship ITC", "gw_itc": true, "hobby": false}`))
	})
	mux.HandleFunc("/leagues/league-hobby", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Hobby Track", "gw_itc": true, "hobby": true}`))
	})
	mux.HandleFunc("/events/evt-gt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-gt", "name": "A GT", "format": {"teamEvent": false}, "gameSystem": {"id": "gs-40k", "name": "Warhammer 40,000"}}`))
	})
	mux.HandleFunc("/events/evt-rtt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-rtt", "name": "An RTT", "format": {"teamEvent": false}}`))
	})
	mux.HandleFunc("/events/evt-nodate", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-nodate", "name": "No End Date", "format": {"teamEvent": false}}`))
	})
	mux.HandleFunc("/events/evt-team", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-team", "name": "A Team GT", "format": {"teamEvent": true}}`))
	})
	mux.HandleFunc("/events/evt-club-solo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-club-solo", "name": "A Solo RTT With A Club Tag", "format": {"teamEvent": false}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestPlayerStats_AggregatesPlacingHistory(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	server := stubBCPStatsServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newStatsTestEcho(store, client)

	req := httptest.NewRequest(http.MethodGet, "/api/me/stats", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp playerStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}

	if !resp.Linked {
		t.Fatal("linked = false, want true")
	}
	if resp.TotalEvents != 5 {
		t.Errorf("totalEvents = %d, want 5", resp.TotalEvents)
	}
	// Best placing overall is the GT's 2nd... except evt-nodate placed 1st,
	// which is better, so the overall best is 1.
	if resp.BestPlacing == nil || *resp.BestPlacing != 1 {
		t.Errorf("bestPlacing = %v, want 1", resp.BestPlacing)
	}
	if resp.BestPlacingGT == nil || *resp.BestPlacingGT != 2 {
		t.Errorf("bestPlacingGt = %v, want 2 (evt-gt)", resp.BestPlacingGT)
	}
	// evt-rtt (10th), evt-nodate (1st, no end date -> treated as
	// single-day), and evt-club-solo (3rd, has a club/team name but
	// EventInfo says teamEvent: false) all count as RTT, so the best of
	// the three is 1.
	if resp.BestPlacingRTT == nil || *resp.BestPlacingRTT != 1 {
		t.Errorf("bestPlacingRtt = %v, want 1 (evt-nodate, no end date treated as single-day)", resp.BestPlacingRTT)
	}
	// evt-team is the only entry whose EventInfo says teamEvent: true, so
	// it's the only one in this bucket — critically, evt-club-solo (which
	// also has a non-empty Team name on its placing-history row, but
	// isn't actually team-format per its EventInfo) must NOT land here.
	// It's also scored under two leagues (flagship: 5th, hobby track:
	// 2nd) — the flagship one should win, not the numerically-better-
	// looking hobby one.
	if resp.BestPlacingTeams == nil || *resp.BestPlacingTeams != 5 {
		t.Errorf("bestPlacingTeams = %v, want 5 (the flagship-league row, not hobby track's better-looking 2, and not evt-club-solo)", resp.BestPlacingTeams)
	}

	if len(resp.Factions) != 2 {
		t.Fatalf("factions = %+v, want exactly 2", resp.Factions)
	}
	// World Eaters appears in 4 events (evt-gt, evt-rtt, evt-team,
	// evt-club-solo), sorted first (most events).
	if resp.Factions[0].Faction != "World Eaters" || resp.Factions[0].EventCount != 4 {
		t.Errorf("factions[0] = %+v, want World Eaters with count 4", resp.Factions[0])
	}
	if resp.Factions[0].BestPlacing == nil || *resp.Factions[0].BestPlacing != 2 {
		t.Errorf("World Eaters bestPlacing = %v, want 2", resp.Factions[0].BestPlacing)
	}
	if resp.Factions[1].Faction != "Black Legion" || resp.Factions[1].EventCount != 1 {
		t.Errorf("factions[1] = %+v, want Black Legion with count 1", resp.Factions[1])
	}

	if resp.MostRecentGameSystemID != "gs-40k" {
		t.Errorf("mostRecentGameSystemId = %q, want %q (from evt-gt, the most recent entry)", resp.MostRecentGameSystemID, "gs-40k")
	}
}

func TestCanonicalPlacingPerEvent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/leagues/league-flagship", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Flagship ITC", "gw_itc": true, "hobby": false}`))
	})
	mux.HandleFunc("/leagues/league-hobby", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Hobby Track", "gw_itc": true, "hobby": true}`))
	})
	mux.HandleFunc("/leagues/league-legacy", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "2020 Warhammer 40k", "gw_itc": false, "hobby": false}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)

	placing := func(n int) *int { return &n }

	history := []bcp.PlacingHistoryEntry{
		// evt-a: hobby row listed first, flagship second — flagship should
		// still win regardless of row order.
		{EventID: "evt-a", Placing: placing(2), LeagueID: "league-hobby"},
		{EventID: "evt-a", Placing: placing(5), LeagueID: "league-flagship"},
		// evt-b: no flagship row at all (just a legacy league and a
		// leagueless row) — falls back to the first entry seen.
		{EventID: "evt-b", Placing: placing(9), LeagueID: "league-legacy"},
		{EventID: "evt-b", Placing: placing(3)},
		// evt-c: a single row, no league at all — passes through as-is.
		{EventID: "evt-c", Placing: placing(1)},
	}

	got := canonicalPlacingPerEvent(context.Background(), client, history)

	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (one per distinct event id): %+v", len(got), got)
	}
	byEvent := make(map[string]bcp.PlacingHistoryEntry, len(got))
	for _, e := range got {
		byEvent[e.EventID] = e
	}
	if e, ok := byEvent["evt-a"]; !ok || e.Placing == nil || *e.Placing != 5 {
		t.Errorf("evt-a = %+v, want the flagship row (placing 5)", e)
	}
	if e, ok := byEvent["evt-b"]; !ok || e.Placing == nil || *e.Placing != 9 {
		t.Errorf("evt-b = %+v, want the first-seen fallback row (placing 9), since neither row is flagship", e)
	}
	if e, ok := byEvent["evt-c"]; !ok || e.Placing == nil || *e.Placing != 1 {
		t.Errorf("evt-c = %+v, want its only row unchanged", e)
	}

	// Order is preserved (first-seen event order), same contract
	// FetchPlacingHistory's own most-recent-first sort relies on.
	if got[0].EventID != "evt-a" || got[1].EventID != "evt-b" || got[2].EventID != "evt-c" {
		t.Errorf("order = %v, want evt-a, evt-b, evt-c in that order", []string{got[0].EventID, got[1].EventID, got[2].EventID})
	}
}

func TestClassifyEventCategory(t *testing.T) {
	cases := []struct {
		name         string
		entry        bcp.PlacingHistoryEntry
		teamEvent    bool
		wantCategory string
		wantOK       bool
	}{
		{
			"same-day RTT",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01T00:00:00.000Z", EventEndDate: "2026-01-01T00:00:00.000Z"},
			false, categoryRTT, true,
		},
		{
			"two-day GT",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01T00:00:00.000Z", EventEndDate: "2026-01-02T00:00:00.000Z"},
			false, categoryGT, true,
		},
		{
			"three-day GT",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01T00:00:00.000Z", EventEndDate: "2026-01-03T00:00:00.000Z"},
			false, categoryGT, true,
		},
		{
			"missing end date treated as single-day RTT",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01T00:00:00.000Z"},
			false, categoryRTT, true,
		},
		{
			"missing start date can't be classified",
			bcp.PlacingHistoryEntry{EventEndDate: "2026-01-02T00:00:00.000Z"},
			false, "", false,
		},
		{
			"bare-date layout also works",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01", EventEndDate: "2026-01-02"},
			false, categoryGT, true,
		},
		{
			"a team event (per EventInfo.TeamEvent) is its own bucket even though it spans multiple days like a GT",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01T00:00:00.000Z", EventEndDate: "2026-01-03T00:00:00.000Z"},
			true, categoryTeams, true,
		},
		{
			"a team event is its own bucket even with no dates at all",
			bcp.PlacingHistoryEntry{},
			true, categoryTeams, true,
		},
		{
			// The real bug this whole classification was built to catch:
			// a non-empty Team name on the placing-history row (club/roster
			// affiliation) must NOT be treated as team-format on its own —
			// only EventInfo.TeamEvent decides that. See
			// stubBCPStatsServer's evt-club-solo for the same case at the
			// full-endpoint level.
			"a non-empty Team name does NOT imply team-format when EventInfo says otherwise",
			bcp.PlacingHistoryEntry{EventDate: "2026-01-01T00:00:00.000Z", EventEndDate: "2026-01-01T00:00:00.000Z", Team: "Springs Thundercluckers"},
			false, categoryRTT, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCategory, gotOK := classifyEventCategory(tc.entry, tc.teamEvent)
			if gotOK != tc.wantOK || gotCategory != tc.wantCategory {
				t.Errorf("classifyEventCategory(%+v, teamEvent=%v) = (%q, %v), want (%q, %v)", tc.entry, tc.teamEvent, gotCategory, gotOK, tc.wantCategory, tc.wantOK)
			}
		})
	}
}
