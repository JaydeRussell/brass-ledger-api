package bcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// --- ITC ranking (score + rank) --------------------------------------
//
// BCP tracks a season-long, cross-event "ITC Points" ranking per player,
// scoped to a "league" — BCP's current flagship ranking league for the
// game system. Once the league id is known,
// `/v1/placings?...&userId[]={id}` is the one query param on this whole
// API that does filter correctly server-side, making a single, cheap,
// per-player lookup possible without ever pulling the full
// (multi-thousand-row) leaderboard.
//
// Finding *which* league id is the current flagship one used to mean
// searching BCP's game-system-wide `/v1/leagues` list for the newest
// entry flagged `gw_itc` (BCP's marker for its flagship ranking league)
// that isn't the hobby-track variant. That stopped working — confirmed
// live against BCP's real API: the list endpoint no longer returns a
// `gw_itc` field on any record at all (every league comes back with an
// unrelated `itc` boolean instead, always false for the real flagship
// league), and even correcting the field name wouldn't be enough on its
// own — the real current flagship league doesn't sort into the newest
// 99 by startDate either (BCP hard-caps `limit` at 99), buried under a
// large and constantly-growing pool of local/store leagues with newer
// dates. A search-and-sort-and-take-first strategy over that whole pool
// can no longer reliably find it, regardless of field name.
//
// The reliable signal instead: an *event's own* info response already
// lists which league(s) it's scored under (see EventInfo.LeagueIDs,
// populated from BCP's `leagues` array on the event). Fetching each of
// those by id via FetchLeagueInfo (below) still correctly returns
// `gw_itc` — confirmed live: the single-league-by-id endpoint kept the
// field even though the list endpoint dropped it. So resolution is now
// anchored on one specific event's own known leagues (a handful of
// already-cached per-league lookups) rather than a blind search over an
// entire game system's leagues.

// FetchCurrentItcLeagueIDForEvent returns the current flagship ITC
// league id among the given league ids (an event's own EventInfo.
// LeagueIDs), or "" if none of them are the flagship one (gw_itc &&
// !hobby) — e.g. a local RTT scored under only a store/hobby league.
// Each lookup is a small, already-cached FetchLeagueInfo call; one
// failed lookup doesn't abort the search, since a transient error on
// one of an event's leagues shouldn't hide a working one.
func (c *Client) FetchCurrentItcLeagueIDForEvent(ctx context.Context, leagueIDs []string) (string, error) {
	for _, id := range leagueIDs {
		info, err := c.FetchLeagueInfo(ctx, id)
		if err != nil || info == nil {
			continue
		}
		if info.GwItc && !info.Hobby {
			return id, nil
		}
	}
	return "", nil
}

type bcpLeagueInfoResponse struct {
	Name  string `json:"name"`
	GwItc bool   `json:"gw_itc"`
	Hobby bool   `json:"hobby"`
}

func leagueInfoDurableKey(leagueID string) string { return "league:" + leagueID }

func (c *Client) fetchLeagueInfoUncached(ctx context.Context, leagueID string) (*LeagueInfo, error) {
	if c.durable != nil {
		var cached LeagueInfo
		if found, err := c.durable.Get(ctx, leagueInfoDurableKey(leagueID), CacheSchemaVersion, &cached); err == nil && found {
			return &cached, nil
		}
	}

	rawURL := fmt.Sprintf("%s/leagues/%s", c.apiBaseV1, url.PathEscape(leagueID))
	var body bcpLeagueInfoResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return nil, err
	}
	info := &LeagueInfo{Name: body.Name, GwItc: body.GwItc, Hobby: body.Hobby}

	// Unlike an event, a league's own classification (gw_itc/hobby) is
	// effectively permanent reference data the moment it exists — no
	// "ended" gate needed.
	if c.durable != nil {
		c.storeDurably(ctx, leagueInfoDurableKey(leagueID), *info)
	}

	return info, nil
}

// FetchLeagueInfo returns one league's name and gw_itc/hobby flags —
// BCP's own signal for whether a placing scored under this league is
// its flagship, "counts as your real placing" ranking (gw_itc && !hobby)
// as opposed to a parallel Hobby Track score or an old legacy default
// league. See PlacingHistoryEntry's doc comment for why this matters:
// the same event can appear multiple times in a player's placing
// history, once per league it's scored under, each with a different
// placing/points for the same underlying result.
func (c *Client) FetchLeagueInfo(ctx context.Context, leagueID string) (*LeagueInfo, error) {
	return c.leagueInfo.Get(ctx, leagueID)
}

type bcpItcPlacingRecord struct {
	UserID    string   `json:"userId"`
	ITCPoints *float64 `json:"ITCPoints"`
	Placing   *int     `json:"placing"`
	Wins      *int     `json:"wins"`
	Losses    *int     `json:"losses"`
	Ties      *int     `json:"ties"`
}

type bcpItcPlacingsResponse struct {
	Data []bcpItcPlacingRecord `json:"data"`
}

func itcRankingKey(leagueID, bcpUserID string) string {
	return leagueID + ":" + bcpUserID
}

func splitItcRankingKey(key string) (leagueID, bcpUserID string, err error) {
	leagueID, bcpUserID, ok := strings.Cut(key, ":")
	if !ok {
		return "", "", fmt.Errorf("invalid ITC ranking cache key %q", key)
	}
	return leagueID, bcpUserID, nil
}

func itcRankingDurableKey(leagueID, bcpUserID string) string {
	return "itc:" + leagueID + ":" + bcpUserID
}

// itcRankingCacheEntry wraps the stored value so "we asked, and this
// player has no ranking in this league" is a cacheable answer rather
// than indistinguishable from "we never asked".
//
// That distinction is the whole point of storing it: a nil ranking is a
// perfectly normal result (a player who hasn't scored in this league
// yet), and without somewhere to record it, every one of them costs a
// real BCP request on every page load, forever — a tolerated
// non-failure with the same cost as a tolerated failure. See
// CLAUDE.md's note on that, and goneTTL in cache.go for the 404 case.
type itcRankingCacheEntry struct {
	Ranking *ItcRanking `json:"ranking"`
}

func (c *Client) fetchItcRankingUncached(ctx context.Context, leagueID, bcpUserID string) (*ItcRanking, error) {
	durableKey := itcRankingDurableKey(leagueID, bcpUserID)
	if c.durable != nil {
		var stored itcRankingCacheEntry
		found, _, err := c.durable.GetFresh(ctx, durableKey, CacheSchemaVersion, itcRankingRefetchInterval, &stored)
		if err == nil && found {
			return stored.Ranking, nil
		}
	}

	// Built by hand (not url.Values) to keep the literal "userId[]" this
	// endpoint actually honors — url.Values.Encode would percent-encode
	// the brackets, which BCP's API does not treat the same way.
	rawURL := fmt.Sprintf(
		"%s/placings?limit=1&placingsType=player&leagueId=%s&userId[]=%s",
		c.apiBaseV1, url.QueryEscape(leagueID), url.QueryEscape(bcpUserID),
	)

	var body bcpItcPlacingsResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return nil, err
	}

	// Persisted before the "no ranking" early returns below as well as
	// after a real hit — see itcRankingCacheEntry. A failed write just
	// means this gets asked of BCP again next time, which is the old
	// behaviour and not worth failing a request over.
	store := func(r *ItcRanking) {
		if c.durable != nil {
			c.storeDurably(ctx, durableKey, itcRankingCacheEntry{Ranking: r})
		}
	}

	if len(body.Data) == 0 {
		store(nil)
		return nil, nil
	}
	record := body.Data[0]
	// An unmatched userId still comes back as one empty object rather
	// than an empty array or an error — that's this player having no
	// ranking in this league yet, not a failure.
	if record.UserID == "" || record.ITCPoints == nil {
		store(nil)
		return nil, nil
	}

	ranking := &ItcRanking{
		Points:  *record.ITCPoints,
		Placing: record.Placing,
		Wins:    record.Wins,
		Losses:  record.Losses,
		Ties:    record.Ties,
	}
	store(ranking)
	return ranking, nil
}

// FetchItcRanking returns one player's cached, rate-limited ITC ranking
// (points + rank) within a league, or nil if they have no ranking in
// that league.
func (c *Client) FetchItcRanking(ctx context.Context, leagueID, bcpUserID string) (*ItcRanking, error) {
	return c.itcRanking.Get(ctx, itcRankingKey(leagueID, bcpUserID))
}
