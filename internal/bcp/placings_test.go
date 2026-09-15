package bcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPlacingsKey_RoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		eventID   string
		teamEvent bool
	}{
		{"team event", "evt-1", true},
		{"individual event", "evt-2", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := placingsKey(tc.eventID, tc.teamEvent)
			gotEventID, gotTeamEvent, err := splitPlacingsKey(key)
			if err != nil {
				t.Fatalf("splitPlacingsKey(%q) returned error: %v", key, err)
			}
			if gotEventID != tc.eventID || gotTeamEvent != tc.teamEvent {
				t.Errorf("round-trip mismatch for %q: got (%q, %v), want (%q, %v)",
					key, gotEventID, gotTeamEvent, tc.eventID, tc.teamEvent)
			}
		})
	}
}

func TestSplitPlacingsKey_Errors(t *testing.T) {
	if _, _, err := splitPlacingsKey("no-colon-here"); err == nil {
		t.Error("splitPlacingsKey(no colon) succeeded, want an error")
	}
}

func TestFetchPlacings(t *testing.T) {
	cases := []struct {
		name           string
		teamEvent      bool
		body           string
		wantOrder      []string // expected Name order after sorting by Placing
		wantUserIDs    []string // expected BcpUserID order, parallel to wantOrder
		wantFactions   []string // expected Faction order, parallel to wantOrder ("" if omitted)
		wantSubFaction []string // expected SubFaction order, parallel to wantOrder ("" if omitted)
	}{
		{
			name:      "individual placings sorted by placing, ties/missing last",
			teamEvent: false,
			body: `{"active": [
				{"id": "p1", "user": {"id": "u3", "firstName": "Third", "lastName": "Place"}, "placing": 3},
				{"id": "p2", "user": {"id": "u1", "firstName": "First", "lastName": "Place"}, "placing": 1},
				{"id": "p3", "user": {"id": "u4", "firstName": "No", "lastName": "Placing"}},
				{"id": "p4", "user": {"id": "u2", "firstName": "Second", "lastName": "Place"}, "placing": 2}
			]}`,
			wantOrder:   []string{"First Place", "Second Place", "Third Place", "No Placing"},
			wantUserIDs: []string{"u1", "u2", "u3", "u4"},
		},
		{
			// Field names/shape confirmed against a real event's response
			// on 2026-09-14 (see placings.go's bcpPlacingRecord comment):
			// this really is "faction"/"subFaction" on the individual
			// placings-flavored response, same as the plain roster one.
			name:      "individual placings carry faction/subFaction",
			teamEvent: false,
			body: `{"active": [
				{"id": "p1", "user": {"id": "u1", "firstName": "Anna", "lastName": "Adams"}, "placing": 1, "faction": {"name": "Necrons"}, "subFaction": {"name": "Take and Hold"}}
			]}`,
			wantOrder:      []string{"Anna Adams"},
			wantUserIDs:    []string{"u1"},
			wantFactions:   []string{"Necrons"},
			wantSubFaction: []string{"Take and Hold"},
		},
		{
			name:      "team placings use the team name field and have no BcpUserID",
			teamEvent: true,
			body: `{"active": [
				{"id": "t1", "name": "Team B", "placing": 2},
				{"id": "t2", "name": "Team A", "placing": 1}
			]}`,
			wantOrder:   []string{"Team A", "Team B"},
			wantUserIDs: []string{"", ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := "players"
			if tc.teamEvent {
				endpoint = "teamplayers"
			}
			mux := http.NewServeMux()
			mux.HandleFunc(fmt.Sprintf("/events/evt-1/%s", endpoint), jsonHandler(http.StatusOK, tc.body))
			server := httptest.NewServer(mux)
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchPlacings(context.Background(), "evt-1", tc.teamEvent)
			if err != nil {
				t.Fatalf("FetchPlacings returned error: %v", err)
			}
			if len(got) != len(tc.wantOrder) {
				t.Fatalf("FetchPlacings returned %d entries, want %d (%+v)", len(got), len(tc.wantOrder), got)
			}
			for i, entry := range got {
				if entry.Name != tc.wantOrder[i] {
					t.Errorf("entry %d name = %q, want %q", i, entry.Name, tc.wantOrder[i])
				}
				if i < len(tc.wantFactions) && entry.Faction != tc.wantFactions[i] {
					t.Errorf("entry %d faction = %q, want %q", i, entry.Faction, tc.wantFactions[i])
				}
				if i < len(tc.wantSubFaction) && entry.SubFaction != tc.wantSubFaction[i] {
					t.Errorf("entry %d subFaction = %q, want %q", i, entry.SubFaction, tc.wantSubFaction[i])
				}
				if entry.BcpUserID != tc.wantUserIDs[i] {
					t.Errorf("entry %d BcpUserID = %q, want %q", i, entry.BcpUserID, tc.wantUserIDs[i])
				}
			}
		})
	}
}

// TestInvalidatePlacings mirrors pairings_test.go's
// TestInvalidateRoundPairings — confirms InvalidatePlacings reaches the
// right (event, teamEvent) cache key; the manual-invalidate floor's own
// behavior is covered centrally by cache_test.go's TestCache_Invalidate.
func TestInvalidatePlacings(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1/players", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"active": []}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)

	if _, err := client.FetchPlacings(context.Background(), "evt-1", false); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := client.FetchPlacings(context.Background(), "evt-1", false); err != nil {
		t.Fatalf("second fetch (should be cached): %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d before invalidating, want 1", calls)
	}

	// team=true is a different cache key (and endpoint) than team=false —
	// confirms InvalidatePlacings is scoped to the exact key.
	client.InvalidatePlacings("evt-1", true)
	if _, err := client.FetchPlacings(context.Background(), "evt-1", false); err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d after invalidating team=true, want 1 (unrelated key)", calls)
	}

	client.InvalidatePlacings("evt-1", false)
	if _, err := client.FetchPlacings(context.Background(), "evt-1", false); err != nil {
		t.Fatalf("fourth fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d after an immediate Invalidate, want 1 (should have been throttled)", calls)
	}
}
