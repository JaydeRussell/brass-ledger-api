package bcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
