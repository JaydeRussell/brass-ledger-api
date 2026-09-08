package bcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// --- Pure helpers -----------------------------------------------------

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

// --- Fetch functions, against a stub BCP server ------------------------

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
				PlayerCount: intPtr(64), Circuits: []string{"ITC"},
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

func TestFetchPlayers(t *testing.T) {
	cases := []struct {
		name           string
		playersBody    string
		teamPlayerBody string
		teamPlayerCode int
		wantNames      []string
		wantTeams      []string
	}{
		{
			name: "resolves each player's tournament team and skips players with no submitted list",
			playersBody: `{"active": [
				{"id": "p1", "user": {"id": "u1", "firstName": "Anna", "lastName": "Adams"}, "teamPlayerId": "tp1", "faction": {"name": "Necrons"}, "listId": "l1"},
				{"id": "p2", "user": {"id": "u2", "firstName": "Bob", "lastName": "Baker"}, "listUrl": "/lists/2", "faction": {"name": "Orks"}},
				{"id": "p3", "user": {"id": "u3", "firstName": "No", "lastName": "List"}, "faction": {"name": "Tau"}}
			]}`,
			teamPlayerBody: `{"active": [{"id": "tp1", "name": "Master Crafted"}]}`,
			teamPlayerCode: http.StatusOK,
			wantNames:      []string{"Anna Adams", "Bob Baker"},
			wantTeams:      []string{"Master Crafted", ""},
		},
		{
			name:           "a missing /teamplayers endpoint (singles event) doesn't fail the whole request",
			playersBody:    `{"active": [{"id": "p1", "user": {"firstName": "Solo", "lastName": "Player"}, "faction": {"name": "Necrons"}, "listId": "l1"}]}`,
			teamPlayerBody: `not found`,
			teamPlayerCode: http.StatusNotFound,
			wantNames:      []string{"Solo Player"},
			wantTeams:      []string{""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/events/evt-1/players", jsonHandler(http.StatusOK, tc.playersBody))
			mux.HandleFunc("/events/evt-1/teamplayers", jsonHandler(tc.teamPlayerCode, tc.teamPlayerBody))
			server := httptest.NewServer(mux)
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchPlayers(context.Background(), "evt-1")
			if err != nil {
				t.Fatalf("FetchPlayers returned error: %v", err)
			}
			if len(got) != len(tc.wantNames) {
				t.Fatalf("FetchPlayers returned %d players, want %d (%+v)", len(got), len(tc.wantNames), got)
			}
			for i, p := range got {
				if p.Name != tc.wantNames[i] {
					t.Errorf("player %d name = %q, want %q", i, p.Name, tc.wantNames[i])
				}
				if p.Team != tc.wantTeams[i] {
					t.Errorf("player %d team = %q, want %q", i, p.Team, tc.wantTeams[i])
				}
			}
		})
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

func TestFetchCurrentItcLeagueID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "picks the flagship (gw_itc, non-hobby) league",
			body: `{"data": [
				{"id": "hobby-league", "gw_itc": true, "hobby": true},
				{"id": "not-itc-league", "gw_itc": false, "hobby": false},
				{"id": "flagship-league", "gw_itc": true, "hobby": false}
			]}`,
			want: "flagship-league",
		},
		{
			name: "no matching league resolves to empty, not an error",
			body: `{"data": [{"id": "other", "gw_itc": false, "hobby": false}]}`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(jsonHandler(http.StatusOK, tc.body))
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchCurrentItcLeagueID(context.Background(), "gs-1")
			if err != nil {
				t.Fatalf("FetchCurrentItcLeagueID returned error: %v", err)
			}
			if got != tc.want {
				t.Errorf("FetchCurrentItcLeagueID = %q, want %q", got, tc.want)
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
			want: &ItcRanking{Points: 1234.5, Placing: intPtr(7), Wins: intPtr(5), Losses: intPtr(2), Ties: intPtr(1)},
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

// --- test helpers -------------------------------------------------------

func intPtr(v int) *int { return &v }

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
