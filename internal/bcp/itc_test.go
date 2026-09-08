package bcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
