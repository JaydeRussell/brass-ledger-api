package bcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

// TestFetchPlayers_Disposition covers Player.Disposition, populated only
// when BCP's subFaction value is an exact match for one of 40k 11th
// edition's five Force Dispositions (see forceDispositions in types.go) —
// confirmed live against a real event where BCP overloads the same
// subFaction slot for this. An event not using Force Disposition
// missions has a real sub-faction name there instead, which must pass
// through to SubFaction unaffected and leave Disposition empty.
func TestFetchPlayers_Disposition(t *testing.T) {
	cases := []struct {
		name            string
		subFaction      string
		wantSubFaction  string
		wantDisposition string
	}{
		{
			name:            "subFaction holding a Force Disposition value is also surfaced as Disposition",
			subFaction:      "Purge the Foe",
			wantSubFaction:  "Purge the Foe",
			wantDisposition: "Purge the Foe",
		},
		{
			name:            "a genuine sub-faction name is left alone and Disposition stays empty",
			subFaction:      "Ultramarines",
			wantSubFaction:  "Ultramarines",
			wantDisposition: "",
		},
		{
			name:            "no subFaction at all",
			subFaction:      "",
			wantSubFaction:  "",
			wantDisposition: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			playersBody := `{"active": [
				{"id": "p1", "user": {"id": "u1", "firstName": "Anna", "lastName": "Adams"}, "faction": {"name": "Space Marines"}, "subFaction": {"name": "` + tc.subFaction + `"}, "listId": "l1"}
			]}`
			mux.HandleFunc("/events/evt-1/players", jsonHandler(http.StatusOK, playersBody))
			mux.HandleFunc("/events/evt-1/teamplayers", jsonHandler(http.StatusNotFound, "not found"))
			server := httptest.NewServer(mux)
			defer server.Close()
			client := newTestClient(server)

			got, err := client.FetchPlayers(context.Background(), "evt-1")
			if err != nil {
				t.Fatalf("FetchPlayers returned error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("FetchPlayers returned %d players, want 1 (%+v)", len(got), got)
			}
			if got[0].SubFaction != tc.wantSubFaction {
				t.Errorf("SubFaction = %q, want %q", got[0].SubFaction, tc.wantSubFaction)
			}
			if got[0].Disposition != tc.wantDisposition {
				t.Errorf("Disposition = %q, want %q", got[0].Disposition, tc.wantDisposition)
			}
		})
	}
}
