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
