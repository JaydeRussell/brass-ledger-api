package bcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	defaultAPIBaseV1 = "https://newprod-api.bestcoastpairings.com/v1"
	defaultAPIBaseV2 = "https://newprod-api.bestcoastpairings.com/v2"
	defaultSiteBase  = "https://www.bestcoastpairings.com"

	// The header value BCP's own web client sends. Not a secret — a
	// constant baked into their public JS bundle identifying the client
	// type (as opposed to their mobile apps).
	clientIDHeader = "web-app"
)

// Client is this service's connection to BCP's undocumented API,
// wrapping each kind of call in its own Cache (see cache.go) so every
// caller of this service — every browser, not just one — shares the
// same rate-limited, de-duplicated requests to BCP. Each BCP-API area
// (events, players, pairings, placings, ITC rankings, per-user history)
// has its own file — events.go, players.go, pairings.go, placings.go,
// itc.go, history.go — this file just holds the shared Client type,
// its constructors, and the one shared HTTP helper (get).
type Client struct {
	http *http.Client

	// Base URLs — fields rather than package consts so tests can point a
	// Client at a local httptest.Server instead of the real BCP API.
	// NewClient sets these to the real defaults above.
	apiBaseV1 string
	apiBaseV2 string
	siteBase  string

	eventInfo          *Cache[EventInfo]
	players            *Cache[[]Player]
	pairings           *Cache[[]PairingRecord]
	placings           *Cache[[]PlacingEntry]
	itcRanking         *Cache[*ItcRanking]
	playerEventHistory *Cache[[]PlayerEventRecord]
	placingHistory     *Cache[[]PlacingHistoryEntry]
	leagueInfo         *Cache[*LeagueInfo]

	// See durable.go — nil unless SetDurableCache is called.
	durable DurableCache
}

// NewClient builds a ready-to-use Client pointed at the real BCP API.
func NewClient() *Client {
	return newClientWithBases(defaultAPIBaseV1, defaultAPIBaseV2, defaultSiteBase)
}

// NewClientWithBaseURL builds a Client with all three of BCP's base URLs
// (the v1 API, the v2 API, and the public site) pointed at the same
// base — for tests (in this package or callers like internal/api) that
// stand up a single stub HTTP server rather than reaching the real BCP
// API. Production code should use NewClient instead.
func NewClientWithBaseURL(base string) *Client {
	return newClientWithBases(base, base, base)
}

// newClientWithBases builds a Client against arbitrary base URLs — the
// real BCP ones in production (see NewClient), or a test's
// httptest.Server otherwise.
func newClientWithBases(apiBaseV1, apiBaseV2, siteBase string) *Client {
	c := &Client{
		http:      &http.Client{Timeout: 15 * time.Second},
		apiBaseV1: apiBaseV1,
		apiBaseV2: apiBaseV2,
		siteBase:  siteBase,
	}

	c.eventInfo = NewCache(func(ctx context.Context, eventID string) (EventInfo, error) {
		return c.fetchEventInfoUncached(ctx, eventID)
	})
	c.players = NewCache(func(ctx context.Context, eventID string) ([]Player, error) {
		return c.fetchPlayersUncached(ctx, eventID)
	})
	// Keyed by "eventId:pairingType:round" — see FetchRoundPairings.
	c.pairings = NewCache(func(ctx context.Context, key string) ([]PairingRecord, error) {
		eventID, pairingType, round, err := splitPairingsKey(key)
		if err != nil {
			return nil, err
		}
		return c.fetchRoundPairingsUncached(ctx, eventID, pairingType, round)
	})
	// Keyed by "eventId:team" or "eventId:individual" — see FetchPlacings.
	c.placings = NewCache(func(ctx context.Context, key string) ([]PlacingEntry, error) {
		eventID, teamEvent, err := splitPlacingsKey(key)
		if err != nil {
			return nil, err
		}
		return c.fetchPlacingsUncached(ctx, eventID, teamEvent)
	})
	// Keyed by "leagueId:bcpUserId" — see FetchItcRanking.
	c.itcRanking = NewCache(func(ctx context.Context, key string) (*ItcRanking, error) {
		leagueID, bcpUserID, err := splitItcRankingKey(key)
		if err != nil {
			return nil, err
		}
		return c.fetchItcRankingUncached(ctx, leagueID, bcpUserID)
	})
	c.playerEventHistory = NewCache(func(ctx context.Context, bcpUserID string) ([]PlayerEventRecord, error) {
		return c.fetchPlayerEventHistoryUncached(ctx, bcpUserID)
	})
	c.placingHistory = NewCache(func(ctx context.Context, bcpUserID string) ([]PlacingHistoryEntry, error) {
		return c.fetchPlacingHistoryUncached(ctx, bcpUserID)
	})
	// Leagues are a small, shared set reused across every event and every
	// user of this app (there are only a handful of leagues per game
	// system per year), so this cache is unusually effective — unlike
	// most of this client's other caches, a leagueId's gw_itc/hobby flags
	// aren't even user-specific, so one lookup here effectively serves
	// every account that has a placing under that league.
	c.leagueInfo = NewCache(func(ctx context.Context, leagueID string) (*LeagueInfo, error) {
		return c.fetchLeagueInfoUncached(ctx, leagueID)
	})

	return c
}

func (c *Client) get(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("client-id", clientIDHeader)

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", rawURL, err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("BCP request to %s failed: HTTP %d", rawURL, res.StatusCode)
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", rawURL, err)
	}
	return nil
}
