package bcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"all present", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"some empty", []string{"a", "", "c"}, []string{"a", "c"}},
		{"all empty", []string{"", "", ""}, []string{}},
		{"no args", nil, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nonEmpty(tc.in...)
			if len(got) != len(tc.want) {
				t.Fatalf("nonEmpty(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("nonEmpty(%v)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// locFields is shorthand for the anonymous struct type formatLocation
// takes, so each table row can build one inline.
type locFields = struct {
	Name             string `json:"name"`
	City             string `json:"city"`
	State            string `json:"state"`
	Zip              string `json:"zip"`
	Country          string `json:"country"`
	StreetNum        string `json:"streetNum"`
	StreetName       string `json:"streetName"`
	FormattedAddress string `json:"formatted_address"`
}

func TestFormatLocation(t *testing.T) {
	cases := []struct {
		name string
		loc  *locFields
		want string
	}{
		{"nil location", nil, ""},
		{"empty location", &locFields{}, ""},
		{
			name: "prefers a pre-formatted address when present",
			loc: &locFields{
				FormattedAddress: "123 Main St, Springfield",
				Name:             "Ignored Venue Name",
			},
			want: "123 Main St, Springfield",
		},
		{
			name: "builds from parts when there's no formatted address",
			loc: &locFields{
				Name:       "Games Workshop",
				StreetNum:  "123",
				StreetName: "Main St",
				City:       "Springfield",
				State:      "IL",
				Zip:        "62704",
				Country:    "USA",
			},
			want: "Games Workshop, 123 Main St, Springfield, IL 62704, USA",
		},
		{
			name: "omits missing parts rather than leaving gaps",
			loc: &locFields{
				City:    "Springfield",
				Country: "USA",
			},
			want: "Springfield, USA",
		},
		{
			name: "city with no state, and no zip",
			loc: &locFields{
				Name: "Venue",
				City: "Springfield",
			},
			want: "Venue, Springfield",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatLocation(tc.loc)
			if got != tc.want {
				t.Errorf("formatLocation(%+v) = %q, want %q", tc.loc, got, tc.want)
			}
		})
	}
}

func TestFetchEventInfo(t *testing.T) {
	cases := []struct {
		name       string
		respStatus int
		respBody   string
		wantErr    bool
		want       EventInfo
	}{
		{
			name:       "full event, resolves organizer from Tournament Organizer role",
			respStatus: http.StatusOK,
			respBody: `{
				"id": "evt-1", "name": "Challengers Cup",
				"format": {"teamEvent": true},
				"status": {"started": true, "ended": false, "currentRound": 3, "numberOfRounds": 5},
				"dates": {"start": "2026-01-01", "end": "2026-01-02"},
				"location": {"formatted_address": "Somewhere, USA"},
				"owner": {"firstName": "Owner", "lastName": "Person"},
				"eventUsers": [
					{"firstName": "Jane", "lastName": "TO", "role": {"name": "Tournament Organizer"}}
				],
				"playerCounts": {"total": 64},
				"gameSystem": {"id": "gs-1", "name": "Warhammer 40,000"},
				"leagues": [{"name": "ITC"}, {"name": ""}]
			}`,
			want: EventInfo{
				ID: "evt-1", Name: "Challengers Cup", TeamEvent: true,
				Started: true, Ended: false, CurrentRound: 3, NumberOfRounds: 5,
				GameSystem: "Warhammer 40,000", GameSystemID: "gs-1",
				StartDate: "2026-01-01", EndDate: "2026-01-02",
				Location: "Somewhere, USA", Organizer: "Jane TO",
				PlayerCount: new(64), Circuits: []string{"ITC"},
			},
		},
		{
			// Regression test: a stray top-level "teamEvent" (the shape
			// BCP's *v1* events endpoint actually uses — a genuinely
			// different, unrelated API version from the /v2 endpoint this
			// client calls) must NOT be picked up in place of the real
			// nested format.teamEvent this endpoint uses. Caught once by
			// testing against v1 by mistake instead of the exact endpoint
			// this client hits — worth keeping this case so it can't
			// silently regress the other way either.
			name:       "a stray top-level teamEvent (the v1 shape) is ignored — only format.teamEvent counts",
			respStatus: http.StatusOK,
			respBody:   `{"id": "evt-3", "name": "Ignore Top-Level", "teamEvent": true, "format": {"teamEvent": false}}`,
			want:       EventInfo{ID: "evt-3", Name: "Ignore Top-Level", TeamEvent: false, Circuits: []string{}},
		},
		{
			name:       "falls back to the event owner when no Tournament Organizer role is present",
			respStatus: http.StatusOK,
			respBody:   `{"id": "evt-2", "name": "Local RTT", "owner": {"firstName": "Owner", "lastName": "Only"}}`,
			want: EventInfo{
				ID: "evt-2", Name: "Local RTT", Organizer: "Owner Only",
				Circuits: []string{},
			},
		},
		{
			name:       "missing id/name fall back to the requested id and a placeholder name",
			respStatus: http.StatusOK,
			respBody:   `{}`,
			want:       EventInfo{ID: "requested-id", Name: "Unnamed event", Circuits: []string{}},
		},
		{
			name:       "non-2xx upstream status is an error",
			respStatus: http.StatusInternalServerError,
			respBody:   `{}`,
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(jsonHandler(tc.respStatus, tc.respBody))
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchEventInfo(context.Background(), "requested-id")
			if tc.wantErr {
				if err == nil {
					t.Fatal("FetchEventInfo succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchEventInfo returned error: %v", err)
			}
			if !eventInfoEqual(got, tc.want) {
				t.Errorf("FetchEventInfo = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestFetchEventInfo_LeagueIDs covers EventInfo.LeagueIDs specifically —
// eventInfoEqual (used by TestFetchEventInfo above) compares via JSON
// marshaling, which is blind to this field since it's deliberately
// json:"-" (backend-internal only, see its doc comment), so it needs
// its own direct check. This is the field FetchCurrentItcLeagueIDForEvent
// is anchored on, so it's worth covering on its own.
func TestFetchEventInfo_LeagueIDs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "captures each league's id alongside its name",
			body: `{"id": "evt-1", "name": "Test Cup", "leagues": [
				{"id": "league-a", "name": "Warhammer Global Rankings 2026"},
				{"id": "league-b", "name": "Some Store League"}
			]}`,
			want: []string{"league-a", "league-b"},
		},
		{
			name: "a league entry with no id is skipped",
			body: `{"id": "evt-1", "name": "Test Cup", "leagues": [{"name": "No Id Here"}]}`,
			want: []string{},
		},
		{
			name: "no leagues at all resolves to an empty slice, not nil",
			body: `{"id": "evt-1", "name": "Test Cup"}`,
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(jsonHandler(http.StatusOK, tc.body))
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchEventInfo(context.Background(), "evt-1")
			if err != nil {
				t.Fatalf("FetchEventInfo returned error: %v", err)
			}
			if len(got.LeagueIDs) != len(tc.want) {
				t.Fatalf("LeagueIDs = %v, want %v", got.LeagueIDs, tc.want)
			}
			for i := range got.LeagueIDs {
				if got.LeagueIDs[i] != tc.want[i] {
					t.Errorf("LeagueIDs[%d] = %q, want %q", i, got.LeagueIDs[i], tc.want[i])
				}
			}
		})
	}
}

func TestInvalidateEventInfo(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"id": "evt-1", "name": "Still Going", "status": {"started": true, "ended": false}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(server)

	if _, err := client.FetchEventInfo(context.Background(), "evt-1"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := client.FetchEventInfo(context.Background(), "evt-1"); err != nil {
		t.Fatalf("second fetch (should be cached): %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d before invalidating, want 1", calls)
	}

	// Same throttle-not-retested reasoning as TestInvalidatePlayerEventHistory
	// — this just confirms InvalidateEventInfo reaches the right cache key.
	client.InvalidateEventInfo("evt-1")
	if _, err := client.FetchEventInfo(context.Background(), "evt-1"); err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d after an immediate Invalidate, want 1 (should have been throttled)", calls)
	}
}
