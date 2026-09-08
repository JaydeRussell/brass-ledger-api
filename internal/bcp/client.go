package bcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
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
// same rate-limited, de-duplicated requests to BCP.
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
	itcLeagueID        *Cache[string]
	itcRanking         *Cache[*ItcRanking]
	playerEventHistory *Cache[[]PlayerEventRecord]
	placingHistory     *Cache[[]PlacingHistoryEntry]
	leagueInfo         *Cache[*LeagueInfo]
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
	c.itcLeagueID = NewCache(func(ctx context.Context, gameSystemID string) (string, error) {
		return c.fetchCurrentItcLeagueIDUncached(ctx, gameSystemID)
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

// --- Event metadata ---------------------------------------------------

type bcpEventInfoResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Nested under "format" — confirmed against the real /v2/events/{id}
	// response (the endpoint this client actually calls, via apiBaseV2).
	// Don't "fix" this to a top-level teamEvent field: BCP's *v1* events
	// endpoint does put it top-level, and it's easy to test against that
	// one by mistake (a real mistake made once already, caught by
	// re-verifying against the exact endpoint/query this client uses
	// rather than the API in general) — v1 and v2 genuinely disagree on
	// this field's shape.
	Format struct {
		TeamEvent bool `json:"teamEvent"`
	} `json:"format"`
	Status struct {
		Started        bool `json:"started"`
		Ended          bool `json:"ended"`
		CurrentRound   int  `json:"currentRound"`
		NumberOfRounds int  `json:"numberOfRounds"`
	} `json:"status"`
	Dates struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"dates"`
	Location struct {
		Name             string `json:"name"`
		City             string `json:"city"`
		State            string `json:"state"`
		Zip              string `json:"zip"`
		Country          string `json:"country"`
		StreetNum        string `json:"streetNum"`
		StreetName       string `json:"streetName"`
		FormattedAddress string `json:"formatted_address"`
	} `json:"location"`
	Owner struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"owner"`
	EventUsers []struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
		Role      struct {
			Name string `json:"name"`
		} `json:"role"`
	} `json:"eventUsers"`
	PlayerCounts struct {
		Total *int `json:"total"`
	} `json:"playerCounts"`
	TeamPlayerCounts struct {
		Total *int `json:"total"`
	} `json:"teamPlayerCounts"`
	CountLabel  string `json:"countLabel"`
	CountString string `json:"countString"`
	GameSystem  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"gameSystem"`
	Leagues []struct {
		Name string `json:"name"`
	} `json:"leagues"`
}

func formatLocation(loc *struct {
	Name             string `json:"name"`
	City             string `json:"city"`
	State            string `json:"state"`
	Zip              string `json:"zip"`
	Country          string `json:"country"`
	StreetNum        string `json:"streetNum"`
	StreetName       string `json:"streetName"`
	FormattedAddress string `json:"formatted_address"`
}) string {
	if loc == nil {
		return ""
	}
	if loc.FormattedAddress != "" {
		return loc.FormattedAddress
	}
	street := strings.TrimSpace(strings.Join(nonEmpty(loc.StreetNum, loc.StreetName), " "))
	cityState := strings.Join(nonEmpty(loc.City, loc.State), ", ")
	parts := nonEmpty(loc.Name, street, strings.TrimSpace(strings.Join(nonEmpty(cityState, loc.Zip), " ")), loc.Country)
	return strings.Join(parts, ", ")
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (c *Client) fetchEventInfoUncached(ctx context.Context, eventID string) (EventInfo, error) {
	var body bcpEventInfoResponse
	rawURL := fmt.Sprintf("%s/events/%s?role=true", c.apiBaseV2, url.PathEscape(eventID))
	if err := c.get(ctx, rawURL, &body); err != nil {
		return EventInfo{}, err
	}

	id := body.ID
	if id == "" {
		id = eventID
	}
	name := body.Name
	if name == "" {
		name = "Unnamed event"
	}

	organizer := ""
	for _, u := range body.EventUsers {
		if u.Role.Name == "Tournament Organizer" {
			organizer = strings.TrimSpace(u.FirstName + " " + u.LastName)
			break
		}
	}
	if organizer == "" {
		organizer = strings.TrimSpace(body.Owner.FirstName + " " + body.Owner.LastName)
	}

	playerCount := body.PlayerCounts.Total
	if playerCount == nil {
		playerCount = body.TeamPlayerCounts.Total
	}

	circuits := make([]string, 0, len(body.Leagues))
	for _, l := range body.Leagues {
		if l.Name != "" {
			circuits = append(circuits, l.Name)
		}
	}

	return EventInfo{
		ID:                id,
		Name:              name,
		TeamEvent:         body.Format.TeamEvent,
		Started:           body.Status.Started,
		Ended:             body.Status.Ended,
		CurrentRound:      body.Status.CurrentRound,
		NumberOfRounds:    body.Status.NumberOfRounds,
		Description:       body.Description,
		GameSystem:        body.GameSystem.Name,
		GameSystemID:      body.GameSystem.ID,
		StartDate:         body.Dates.Start,
		EndDate:           body.Dates.End,
		Location:          formatLocation(&body.Location),
		Organizer:         organizer,
		RegistrationLabel: body.CountLabel,
		RegistrationCount: body.CountString,
		PlayerCount:       playerCount,
		Circuits:          circuits,
	}, nil
}

// FetchEventInfo returns cached, rate-limited event metadata.
func (c *Client) FetchEventInfo(ctx context.Context, eventID string) (EventInfo, error) {
	return c.eventInfo.Get(ctx, eventID)
}

// --- Players / rosters -------------------------------------------------

type bcpPlayerRecord struct {
	ID   string `json:"id"`
	User struct {
		ID        string `json:"id"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"user"`
	Team struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	TeamPlayerID string `json:"teamPlayerId"`
	Faction      struct {
		Name string `json:"name"`
	} `json:"faction"`
	SubFaction struct {
		Name string `json:"name"`
	} `json:"subFaction"`
	ListID  string `json:"listId"`
	ListURL string `json:"listUrl"`
}

type bcpPlayersResponse struct {
	Active []bcpPlayerRecord `json:"active"`
}

type bcpTeamPlayerRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type bcpTeamPlayersResponse struct {
	Active []bcpTeamPlayerRecord `json:"active"`
}

// fetchPlayersUncached mirrors the frontend's original fetchBcpPlayersUncached:
// a "has a submitted list" check stands in for "is a real roster entry"
// (check-in doesn't happen until day-of, so filtering on that would hide
// real rosters submitted days earlier), and each player's actual
// per-event tournament team name is resolved via teamPlayerId against
// the separate /teamplayers collection.
func (c *Client) fetchPlayersUncached(ctx context.Context, eventID string) ([]Player, error) {
	encodedID := url.PathEscape(eventID)

	var playersBody bcpPlayersResponse
	if err := c.get(ctx, fmt.Sprintf("%s/events/%s/players", c.apiBaseV1, encodedID), &playersBody); err != nil {
		return nil, err
	}

	// /teamplayers 404s (or is simply empty) for events with no team
	// concept at all — that's expected for singles events, not an error,
	// so a failure here is swallowed rather than propagated.
	teamNameByID := make(map[string]string)
	var teamPlayersBody bcpTeamPlayersResponse
	if err := c.get(ctx, fmt.Sprintf("%s/events/%s/teamplayers", c.apiBaseV1, encodedID), &teamPlayersBody); err == nil {
		for _, tp := range teamPlayersBody.Active {
			if tp.Name != "" {
				teamNameByID[tp.ID] = tp.Name
			}
		}
	}

	players := make([]Player, 0, len(playersBody.Active))
	for _, r := range playersBody.Active {
		if r.ListID == "" && r.ListURL == "" {
			continue // no submitted list yet
		}

		name := strings.TrimSpace(r.User.FirstName + " " + r.User.LastName)
		if name == "" {
			name = "Unknown player"
		}

		faction := r.Faction.Name
		if faction == "" {
			faction = "Unknown"
		}

		list := ""
		if r.ListURL != "" {
			list = c.siteBase + r.ListURL
		}

		var teamName string
		if r.TeamPlayerID != "" {
			teamName = teamNameByID[r.TeamPlayerID]
		}

		players = append(players, Player{
			ID:           r.ID,
			Name:         name,
			Faction:      faction,
			SubFaction:   r.SubFaction.Name,
			Team:         teamName,
			TeamPlayerID: r.TeamPlayerID,
			HomeClub:     r.Team.Name,
			List:         list,
			BcpUserID:    r.User.ID,
		})
	}

	return players, nil
}

// FetchPlayers returns cached, rate-limited roster data for an event —
// every registered player with a submitted list, individual or team.
func (c *Client) FetchPlayers(ctx context.Context, eventID string) ([]Player, error) {
	return c.players.Get(ctx, eventID)
}

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
	q := url.Values{}
	q.Set("pairingType", pairingType)
	q.Set("round", strconv.Itoa(round))
	rawURL := fmt.Sprintf("%s/events/%s/pairings?%s", c.apiBaseV1, url.PathEscape(eventID), q.Encode())

	var body bcpPairingsResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return nil, err
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

// --- Placings -------------------------------------------------------------

type bcpPlacingRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"` // team events
	User struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"user"` // individual events
	Placing *int `json:"placing"`
	Metrics []struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	} `json:"metrics"`
}

type bcpPlacingsResponse struct {
	Active []bcpPlacingRecord `json:"active"`
}

func placingsKey(eventID string, teamEvent bool) string {
	if teamEvent {
		return eventID + ":team"
	}
	return eventID + ":individual"
}

func splitPlacingsKey(key string) (eventID string, teamEvent bool, err error) {
	idx := strings.LastIndex(key, ":")
	if idx < 0 {
		return "", false, fmt.Errorf("invalid placings cache key %q", key)
	}
	return key[:idx], key[idx+1:] == "team", nil
}

func (c *Client) fetchPlacingsUncached(ctx context.Context, eventID string, teamEvent bool) ([]PlacingEntry, error) {
	endpoint := "players"
	if teamEvent {
		endpoint = "teamplayers"
	}
	rawURL := fmt.Sprintf("%s/events/%s/%s?placings=true", c.apiBaseV1, url.PathEscape(eventID), endpoint)

	var body bcpPlacingsResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return nil, err
	}

	entries := make([]PlacingEntry, 0, len(body.Active))
	for _, r := range body.Active {
		name := r.Name
		if !teamEvent {
			name = strings.TrimSpace(r.User.FirstName + " " + r.User.LastName)
		}
		if name == "" {
			if teamEvent {
				name = "Unknown team"
			} else {
				name = "Unknown player"
			}
		}

		metrics := make([]PlacingMetric, 0, len(r.Metrics))
		for _, m := range r.Metrics {
			metrics = append(metrics, PlacingMetric{Name: m.Name, Value: m.Value})
		}

		entries = append(entries, PlacingEntry{
			ID:      r.ID,
			Name:    name,
			Placing: r.Placing,
			Metrics: metrics,
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		pi, pj := entries[i].Placing, entries[j].Placing
		switch {
		case pi == nil && pj == nil:
			return false
		case pi == nil:
			return false
		case pj == nil:
			return true
		default:
			return *pi < *pj
		}
	})

	return entries, nil
}

// FetchPlacings returns cached, rate-limited standings — whatever BCP
// has already computed and published, ranked by its own `placing`
// field. Empty until BCP has actually placed anyone, which typically
// means at least one round has finished.
func (c *Client) FetchPlacings(ctx context.Context, eventID string, teamEvent bool) ([]PlacingEntry, error) {
	return c.placings.Get(ctx, placingsKey(eventID, teamEvent))
}

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

func (c *Client) fetchLeagueInfoUncached(ctx context.Context, leagueID string) (*LeagueInfo, error) {
	rawURL := fmt.Sprintf("%s/leagues/%s", c.apiBaseV1, url.PathEscape(leagueID))
	var body bcpLeagueInfoResponse
	if err := c.get(ctx, rawURL, &body); err != nil {
		return nil, err
	}
	return &LeagueInfo{Name: body.Name, GwItc: body.GwItc, Hobby: body.Hobby}, nil
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

// --- Per-user event history ("My Events") ------------------------------
//
// Two separate BCP endpoints, combined by the caller (internal/api) to
// classify a user's events into past/present/future:
//
//   - /v1/players?userId=... returns every event a user has ever
//     registered a roster for, but its expanded `event` is only
//     {id, name} — no dates, so it can't say past/present/future on its
//     own.
//   - /v1/eventplacings?userId=... returns final results for every
//     already-concluded event the user has a placing in, WITH dates —
//     any event in this list is unambiguously "Past".
//
// Anything present in the players list but absent from the placings
// list (typically just the 0-3 most recent/current events) needs a
// separate FetchEventInfo call to read Started/Ended and classify it as
// present or future. Both endpoints are cursor-paginated via nextKey.

// maxHistoryPages bounds how many pages of a user's history this will
// fetch in one go — a safety cap (10 pages * limit 100 = up to 1,000
// records), not a limit anyone should realistically hit, so a
// misbehaving account can't turn one request into an unbounded crawl of
// BCP's API.
const maxHistoryPages = 10

// decodeNextKey normalizes a paginated response's nextKey field into the
// string form the *next* request's "nextKey" query param expects.
// BCP is inconsistent here (confirmed against real responses, not just
// docs): most pages return it pre-encoded as a JSON string (already the
// base64 form the query param wants, passed straight through), but at
// least one observed /v1/eventplacings response instead returned the
// raw cursor object itself — e.g. {"type":"query","value":{...}} — with
// no encoding at all. Decoding the pre-encoded string form confirms
// it's exactly base64(that same JSON object), so the fix is: whatever
// isn't already a JSON string gets base64-std-encoded here before it's
// used as the next page's cursor. An empty/absent/null nextKey means no
// more pages, reported as "".
func decodeNextKey(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	// Not a JSON string — assume it's the raw cursor object BCP forgot to
	// encode, and encode it ourselves the same way the string form
	// decodes to.
	return base64.StdEncoding.EncodeToString(raw), nil
}

type bcpPlayerEventRecord struct {
	EventID string `json:"eventId"`
	Event   struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"event"`
	CheckedIn bool `json:"checkedIn"`
	Dropped   bool `json:"dropped"`
}

type bcpPlayersByUserResponse struct {
	Data []bcpPlayerEventRecord `json:"data"`
	// json.RawMessage, not string: see decodeNextKey's doc comment for
	// why this field's actual JSON shape isn't reliably a string.
	NextKey json.RawMessage `json:"nextKey,omitempty"`
}

func (c *Client) fetchPlayerEventHistoryUncached(ctx context.Context, bcpUserID string) ([]PlayerEventRecord, error) {
	var records []PlayerEventRecord
	nextKey := ""
	for page := 0; page < maxHistoryPages; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		q.Set("userId", bcpUserID)
		q.Add("expand[]", "event")
		if nextKey != "" {
			q.Set("nextKey", nextKey)
		}
		rawURL := fmt.Sprintf("%s/players?%s", c.apiBaseV1, q.Encode())

		var body bcpPlayersByUserResponse
		if err := c.get(ctx, rawURL, &body); err != nil {
			return nil, err
		}
		for _, r := range body.Data {
			eventID := r.EventID
			if eventID == "" {
				eventID = r.Event.ID
			}
			if eventID == "" {
				continue // no event reference at all — nothing to show
			}
			eventName := r.Event.Name
			if eventName == "" {
				eventName = "Unnamed event"
			}
			records = append(records, PlayerEventRecord{
				EventID:   eventID,
				EventName: eventName,
				CheckedIn: r.CheckedIn,
				Dropped:   r.Dropped,
			})
		}
		next, err := decodeNextKey(body.NextKey)
		if err != nil {
			return nil, fmt.Errorf("decoding nextKey from %s: %w", rawURL, err)
		}
		if next == "" {
			break
		}
		nextKey = next
	}
	return records, nil
}

// FetchPlayerEventHistory returns every event a BCP user has ever
// registered a roster for (past, present, or future alike), cached and
// rate-limited per user. Pair with FetchPlacingHistory and, for events
// not covered there, FetchEventInfo to classify each one.
func (c *Client) FetchPlayerEventHistory(ctx context.Context, bcpUserID string) ([]PlayerEventRecord, error) {
	return c.playerEventHistory.Get(ctx, bcpUserID)
}

type bcpPlacingHistoryEventRef struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	EventDate    string `json:"eventDate"`
	EventEndDate string `json:"eventEndDate"`
}

type bcpPlacingHistoryRecord struct {
	Placing  *int                      `json:"placing,omitempty"`
	Points   *float64                  `json:"points,omitempty"`
	Event    bcpPlacingHistoryEventRef `json:"event"`
	LeagueID string                    `json:"leagueId,omitempty"`
	Faction  struct {
		Name string `json:"name"`
	} `json:"faction"`
	Team struct {
		Name string `json:"name"`
	} `json:"team"`
}

type bcpPlacingHistoryResponse struct {
	Data []bcpPlacingHistoryRecord `json:"data"`
	// json.RawMessage, not string: see decodeNextKey's doc comment — this
	// is the endpoint where BCP was actually observed sending the raw,
	// unencoded cursor object instead of a JSON string.
	NextKey json.RawMessage `json:"nextKey,omitempty"`
}

func (c *Client) fetchPlacingHistoryUncached(ctx context.Context, bcpUserID string) ([]PlacingHistoryEntry, error) {
	var entries []PlacingHistoryEntry
	nextKey := ""
	for page := 0; page < maxHistoryPages; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		q.Set("userId", bcpUserID)
		q.Add("expand[]", "event")
		q.Add("expand[]", "team")
		q.Add("expand[]", "army") // BCP's own param name for faction expansion on this endpoint
		if nextKey != "" {
			q.Set("nextKey", nextKey)
		}
		rawURL := fmt.Sprintf("%s/eventplacings?%s", c.apiBaseV1, q.Encode())

		var body bcpPlacingHistoryResponse
		if err := c.get(ctx, rawURL, &body); err != nil {
			return nil, err
		}
		for _, r := range body.Data {
			if r.Event.ID == "" {
				continue // an event that's since been deleted/hidden — skip rather than show a blank entry
			}
			name := r.Event.Name
			if name == "" {
				name = "Unnamed event"
			}
			entries = append(entries, PlacingHistoryEntry{
				EventID:      r.Event.ID,
				EventName:    name,
				EventDate:    r.Event.EventDate,
				EventEndDate: r.Event.EventEndDate,
				Placing:      r.Placing,
				Points:       r.Points,
				Faction:      r.Faction.Name,
				Team:         r.Team.Name,
				LeagueID:     r.LeagueID,
			})
		}
		next, err := decodeNextKey(body.NextKey)
		if err != nil {
			return nil, fmt.Errorf("decoding nextKey from %s: %w", rawURL, err)
		}
		if next == "" {
			break
		}
		nextKey = next
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].EventDate > entries[j].EventDate // most recent first
	})

	return entries, nil
}

// FetchPlacingHistory returns final results for every already-concluded
// event a BCP user has a placing in, cached and rate-limited per user,
// most recent first. Any event returned here is unambiguously "Past".
func (c *Client) FetchPlacingHistory(ctx context.Context, bcpUserID string) ([]PlacingHistoryEntry, error) {
	return c.placingHistory.Get(ctx, bcpUserID)
}
