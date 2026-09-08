package bcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// --- ITC ranking (score + rank) --------------------------------------
//
// See the frontend's original section note (still accurate): BCP tracks
// a season-long, cross-event "ITC Points" ranking per player, scoped to
// a "league" — BCP's current flagship ranking league for the game
// system, found by fetching the most recent leagues for that game
// system and picking the one flagged gw_itc (BCP's own marker for its
// flagship ranking league) that isn't the hobby-track variant. Once the
// league id is known, `/v1/placings?...&userId[]={id}` is the one query
// param on this whole API that does filter correctly server-side, making
// a single, cheap, per-player lookup possible without ever pulling the
// full (multi-thousand-row) leaderboard.

type bcpLeagueRecord struct {
	ID    string `json:"id"`
	GwItc bool   `json:"gw_itc"`
	Hobby bool   `json:"hobby"`
}

type bcpLeaguesResponse struct {
	Data []bcpLeagueRecord `json:"data"`
}

func (c *Client) fetchCurrentItcLeagueIDUncached(ctx context.Context, gameSystemID string) (string, error) {
	q := url.Values{}
	q.Set("limit", "99")
	q.Set("gameSystemId", gameSystemID)
	q.Set("sortAscending", "false")
	q.Set("sortBy", "startDate")
	rawURL := fmt.Sprintf("%s/leagues?%s", c.apiBaseV1, q.Encode())

	var body bcpLeaguesResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return "", err
	}

	for _, l := range body.Data {
		if l.GwItc && !l.Hobby {
			return l.ID, nil
		}
	}
	return "", nil
}

// FetchCurrentItcLeagueID returns BCP's current flagship ITC ranking
// league id for a game system, or "" if it couldn't be resolved (e.g.
// this game system has no such league).
func (c *Client) FetchCurrentItcLeagueID(ctx context.Context, gameSystemID string) (string, error) {
	return c.itcLeagueID.Get(ctx, gameSystemID)
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
		if found, err := c.durable.Get(ctx, leagueInfoDurableKey(leagueID), &cached); err == nil && found {
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
		_ = c.durable.Set(ctx, leagueInfoDurableKey(leagueID), *info)
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
	idx := strings.Index(key, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid ITC ranking cache key %q", key)
	}
	return key[:idx], key[idx+1:], nil
}

func (c *Client) fetchItcRankingUncached(ctx context.Context, leagueID, bcpUserID string) (*ItcRanking, error) {
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

	if len(body.Data) == 0 {
		return nil, nil
	}
	record := body.Data[0]
	// An unmatched userId still comes back as one empty object rather
	// than an empty array or an error — that's this player having no
	// ranking in this league yet, not a failure.
	if record.UserID == "" || record.ITCPoints == nil {
		return nil, nil
	}

	return &ItcRanking{
		Points:  *record.ITCPoints,
		Placing: record.Placing,
		Wins:    record.Wins,
		Losses:  record.Losses,
		Ties:    record.Ties,
	}, nil
}

// FetchItcRanking returns one player's cached, rate-limited ITC ranking
// (points + rank) within a league, or nil if they have no ranking in
// that league.
func (c *Client) FetchItcRanking(ctx context.Context, leagueID, bcpUserID string) (*ItcRanking, error) {
	return c.itcRanking.Get(ctx, itcRankingKey(leagueID, bcpUserID))
}
