package bcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		name      string
		teamEvent bool
		body      string
		wantOrder []string // expected Name order after sorting by Placing
	}{
		{
			name:      "individual placings sorted by placing, ties/missing last",
			teamEvent: false,
			body: `{"active": [
				{"id": "p1", "user": {"firstName": "Third", "lastName": "Place"}, "placing": 3},
				{"id": "p2", "user": {"firstName": "First", "lastName": "Place"}, "placing": 1},
				{"id": "p3", "user": {"firstName": "No", "lastName": "Placing"}},
				{"id": "p4", "user": {"firstName": "Second", "lastName": "Place"}, "placing": 2}
			]}`,
			wantOrder: []string{"First Place", "Second Place", "Third Place", "No Placing"},
		},
		{
			name:      "team placings use the team name field",
			teamEvent: true,
			body: `{"active": [
				{"id": "t1", "name": "Team B", "placing": 2},
				{"id": "t2", "name": "Team A", "placing": 1}
			]}`,
			wantOrder: []string{"Team A", "Team B"},
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
			}
		})
	}
}
