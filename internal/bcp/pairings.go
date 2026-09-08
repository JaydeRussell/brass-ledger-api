package bcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// --- Pairings -----------------------------------------------------------

type bcpPairingsResponse struct {
	Active []PairingRecord `json:"active"`
}

func pairingsKey(eventID, pairingType string, round int) string {
	return fmt.Sprintf("%s:%s:%d", eventID, pairingType, round)
}

func splitPairingsKey(key string) (eventID, pairingType string, round int, err error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("invalid pairings cache key %q", key)
	}
	round, err = strconv.Atoi(parts[2])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid round in pairings cache key %q: %w", key, err)
	}
	return parts[0], parts[1], round, nil
}

// NOTE: BCP's playerId/teamPlayerId query params on this endpoint are NOT
// honored server-side (confirmed by testing against BCP directly, same
// as the frontend's original note) — so every "who did X play in round
// N" lookup fetches the whole round once (cached here) and filters
// client-side (still true — just that "client" is now this service's
// caller, the frontend, rather than BCP).
func (c *Client) fetchRoundPairingsUncached(ctx context.Context, eventID, pairingType string, round int) ([]PairingRecord, error) {
	durableKey := "pairings:" + pairingsKey(eventID, pairingType, round)
	if c.durable != nil {
		var cached []PairingRecord
		if found, err := c.durable.Get(ctx, durableKey, &cached); err == nil && found {
			return cached, nil
		}
	}

	q := url.Values{}
	q.Set("pairingType", pairingType)
	q.Set("round", strconv.Itoa(round))
	rawURL := fmt.Sprintf("%s/events/%s/pairings?%s", c.apiBaseV1, url.PathEscape(eventID), q.Encode())

	var body bcpPairingsResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return nil, err
	}

	// A round's pairings only stop changing once the whole event has
	// concluded — same reasoning (and same already-warm cache) as
	// fetchPlayersUncached above.
	if c.durable != nil {
		if info, err := c.FetchEventInfo(ctx, eventID); err == nil && info.Ended {
			_ = c.durable.Set(ctx, durableKey, body.Active)
		}
	}

	return body.Active, nil
}

// FetchRoundPairings returns every pairing BCP has published for one
// round of one event — cached and rate-limited per
// (event, pairingType, round) triple, and reused across every different
// shape the frontend derives from it (the full board, "my pairings" for
// whoever's followed, and a team pairing's individual boards), so
// browsing a round costs at most one real request to BCP no matter how
// many of those views ask for it.
func (c *Client) FetchRoundPairings(ctx context.Context, eventID, pairingType string, round int) ([]PairingRecord, error) {
	return c.pairings.Get(ctx, pairingsKey(eventID, pairingType, round))
}
