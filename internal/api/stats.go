package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// factionStat is one faction's summary across a player's placing history:
// how many events they brought it to, and their best (lowest) placing
// with it — both derived from data BCP already published per event,
// never computed about a pairing/matchup.
type factionStat struct {
	Faction     string `json:"faction"`
	EventCount  int    `json:"eventCount"`
	BestPlacing *int   `json:"bestPlacing,omitempty"`
}

// placingWithField is a best placing plus the size of the field it was
// achieved in — "8th of 53" reads very differently from a bare "8th",
// and EventInfo.PlayerCount (already resolved for every event by
// eventInfoByID, for TeamEvent/GameSystemID) makes this free to attach.
// FieldSize is nil if that event's PlayerCount was never published.
type placingWithField struct {
	Placing   int  `json:"placing"`
	FieldSize *int `json:"fieldSize,omitempty"`
}

// playerStatsResponse is GET /api/me/stats' body. Linked is false (with
// every other field at its zero value) for an account that hasn't pasted
// a BCP profile yet — same "not linked" vs "linked but nothing yet"
// distinction myEventsResponse makes.
//
// MostRecentEventID is the event id of the player's single most recent
// placing-history entry — the frontend's cue for which event's own
// known leagues to resolve a current ITC league from (via
// /api/itc/leagues/event/:eventId and /api/itc/rankings), anchored on
// one specific event rather than a bare game system id (see
// FetchCurrentItcLeagueIDForEvent's doc comment in internal/bcp/itc.go
// for why a game-system-wide search stopped being reliable). Omitted if
// there's no placing history at all, in which case the frontend just
// doesn't show an ITC score rather than failing the whole stats card.
//
// CompetingSince is the earliest EventDate across the player's whole
// (deduped) history — BCP already publishes it per event; this just
// takes the min.
type playerStatsResponse struct {
	Linked            bool              `json:"linked"`
	TotalEvents       int               `json:"totalEvents"`
	BestPlacing       *placingWithField `json:"bestPlacing,omitempty"`
	BestPlacingRTT    *placingWithField `json:"bestPlacingRtt,omitempty"`
	BestPlacingGT     *placingWithField `json:"bestPlacingGt,omitempty"`
	BestPlacingTeams  *placingWithField `json:"bestPlacingTeams,omitempty"`
	Factions          []factionStat     `json:"factions"`
	MostRecentEventID string            `json:"mostRecentEventId,omitempty"`
	CompetingSince    string            `json:"competingSince,omitempty"`
}

func emptyPlayerStatsResponse(linked bool) playerStatsResponse {
	return playerStatsResponse{Linked: linked, Factions: []factionStat{}}
}

// Event category buckets for the "best placing" split — a team event
// gets its own bucket regardless of how many days it ran, since a GT
// vs RTT split is really about individual-event format; only once an
// event isn't a team event does its day span decide GT vs RTT.
const (
	categoryTeams = "team"
	categoryGT    = "gt"
	categoryRTT   = "rtt"
)

// classifyEventCategory reports which "best placing" bucket a placing-
// history entry belongs in, and whether it could be classified at all.
// teamEvent must come from the event's own EventInfo.TeamEvent (see
// eventInfoByID) — PlacingHistoryEntry's own Team field is NOT a
// reliable signal here despite how it looks: it's the player's
// club/roster affiliation (e.g. "Springs Thundercluckers"), populated
// for that player's events regardless of whether any given one was
// actually run in team format. Confirmed against real data: two solo
// RTTs (10 and 12 players, EventInfo.TeamEvent = false) both still
// carried a non-empty Team name. teamEvent checked first, since it needs
// no date parsing. Otherwise, falls back to the event's day span: two or
// more calendar days is a GT, one is an RTT. A missing or unparseable
// end date is treated as a single day (ok=true, rtt): BCP simply not
// publishing an end date is far more often a one-day event that never
// had one than a multi-day one.
func classifyEventCategory(h bcp.PlacingHistoryEntry, teamEvent bool) (category string, ok bool) {
	if teamEvent {
		return categoryTeams, true
	}
	start, startOK := parseBCPDate(h.EventDate)
	if !startOK {
		return "", false
	}
	end, endOK := parseBCPDate(h.EventEndDate)
	if !endOK {
		return categoryRTT, true
	}
	days := int(end.Sub(start).Hours()/24) + 1
	if days >= 2 {
		return categoryGT, true
	}
	return categoryRTT, true
}

// canonicalPlacingPerEvent collapses a player's raw placing history down
// to at most one entry per event. BCP scores one event under several
// leagues/circuits at once — its flagship ITC ranking, a separate Hobby
// Track scoring, sometimes an old legacy default league too — so the
// same event can appear multiple times here, each with a different
// placing/points, sharing only the EventID (see PlacingHistoryEntry's
// doc comment). Naively aggregating every row would both inflate "how
// many events you've played" and let an unrelated league's placing (e.g.
// a small-pool Hobby Track score) get picked as "the" placing purely for
// being numerically lower.
//
// Prefers whichever entry's league is BCP's flagship, non-hobby ITC
// league (gw_itc && !hobby) — the same definition already used for this
// app's ITC ranking feature (FetchCurrentItcLeagueIDForEvent) — falling back to
// the first entry seen for an event with no flagship-league row at all
// (e.g. a small local RTT with no circuit affiliation, which still
// deserves a placing shown even though it can't be confirmed flagship).
//
// The extra cost is one FetchLeagueInfo call per distinct LeagueID
// encountered, not per event or per row — leagues are a small, shared,
// cached set (a handful per game system per year, and not user-specific
// at all), so this stays well within "fetch only what's needed" even for
// a long history.
func canonicalPlacingPerEvent(ctx context.Context, client bcpClient, history []bcp.PlacingHistoryEntry) []bcp.PlacingHistoryEntry {
	type group struct {
		fallback bcp.PlacingHistoryEntry
		flagship *bcp.PlacingHistoryEntry
	}
	order := make([]string, 0, len(history))
	groups := make(map[string]*group, len(history))

	for _, h := range history {
		g, exists := groups[h.EventID]
		if !exists {
			g = &group{fallback: h}
			groups[h.EventID] = g
			order = append(order, h.EventID)
		}
		if g.flagship != nil || h.LeagueID == "" {
			continue
		}
		info, err := client.FetchLeagueInfo(ctx, h.LeagueID)
		if err != nil || info == nil {
			continue // an unresolvable league just means this row can't win flagship status — not a failed request
		}
		if info.GwItc && !info.Hobby {
			entry := h
			g.flagship = &entry
		}
	}

	result := make([]bcp.PlacingHistoryEntry, 0, len(order))
	for _, id := range order {
		g := groups[id]
		if g.flagship != nil {
			result = append(result, *g.flagship)
		} else {
			result = append(result, g.fallback)
		}
	}
	return result
}

// eventInfoByID resolves EventInfo for every distinct event in a
// (already deduped) placing history, keyed by EventID — the only
// reliable source of TeamEvent (see classifyEventCategory's doc comment)
// and, as a side benefit, of GameSystemID for every event rather than
// just the most recent one. One cached FetchEventInfo call per distinct
// event; an event whose info fails to load just can't be classified or
// counted toward a game system, rather than failing the whole request.
func eventInfoByID(ctx context.Context, client bcpClient, history []bcp.PlacingHistoryEntry) map[string]bcp.EventInfo {
	infos := make(map[string]bcp.EventInfo, len(history))
	for _, h := range history {
		if _, ok := infos[h.EventID]; ok {
			continue
		}
		info, err := client.FetchEventInfo(ctx, h.EventID)
		if err != nil {
			continue
		}
		infos[h.EventID] = info
	}
	return infos
}

// computePlayerStats aggregates a player's already-published placing
// history into the summary this endpoint returns — best placing overall
// and split Team/GT/RTT, plus a per-faction breakdown. Every input here
// (Placing, EventDate/EventEndDate, Faction, and each event's TeamEvent
// via infos) is something BCP already computed and published; this only
// re-groups and min()s it, never scores or ranks anything itself.
// Expects history to already be deduped to one entry per event — see
// canonicalPlacingPerEvent, which every real caller runs first.
func computePlayerStats(history []bcp.PlacingHistoryEntry, infos map[string]bcp.EventInfo) playerStatsResponse {
	resp := emptyPlayerStatsResponse(true)
	resp.TotalEvents = len(history)

	factionIndex := make(map[string]int, len(history))

	keepBestPlain := func(best *int, placing int) *int {
		if best == nil || placing < *best {
			p := placing
			return &p
		}
		return best
	}
	keepBestWithField := func(best *placingWithField, placing int, fieldSize *int) *placingWithField {
		if best == nil || placing < best.Placing {
			return &placingWithField{Placing: placing, FieldSize: fieldSize}
		}
		return best
	}

	var earliest time.Time
	for _, h := range history {
		fieldSize := infos[h.EventID].PlayerCount

		if h.Placing != nil {
			resp.BestPlacing = keepBestWithField(resp.BestPlacing, *h.Placing, fieldSize)
			if category, ok := classifyEventCategory(h, infos[h.EventID].TeamEvent); ok {
				switch category {
				case categoryTeams:
					resp.BestPlacingTeams = keepBestWithField(resp.BestPlacingTeams, *h.Placing, fieldSize)
				case categoryGT:
					resp.BestPlacingGT = keepBestWithField(resp.BestPlacingGT, *h.Placing, fieldSize)
				case categoryRTT:
					resp.BestPlacingRTT = keepBestWithField(resp.BestPlacingRTT, *h.Placing, fieldSize)
				}
			}
		}

		if t, ok := parseBCPDate(h.EventDate); ok && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
			resp.CompetingSince = h.EventDate
		}

		if h.Faction == "" {
			continue
		}
		idx, exists := factionIndex[h.Faction]
		if !exists {
			idx = len(resp.Factions)
			factionIndex[h.Faction] = idx
			resp.Factions = append(resp.Factions, factionStat{Faction: h.Faction})
		}
		resp.Factions[idx].EventCount++
		if h.Placing != nil {
			resp.Factions[idx].BestPlacing = keepBestPlain(resp.Factions[idx].BestPlacing, *h.Placing)
		}
	}

	sort.SliceStable(resp.Factions, func(i, j int) bool {
		return resp.Factions[i].EventCount > resp.Factions[j].EventCount
	})

	return resp
}

// StatsHandler wires up the signed-in user's own player-stats summary —
// best placing (overall, GT, RTT, Team) and a per-faction breakdown,
// built on the same FetchPlacingHistory call "My Events" already makes,
// plus two extra passes over that history: one FetchLeagueInfo call per
// distinct league encountered (see canonicalPlacingPerEvent) and one
// FetchEventInfo call per distinct event (see eventInfoByID, needed for
// an accurate Team/GT/RTT split — PlacingHistoryEntry's own Team field
// isn't reliable enough on its own). Both are small, cached, mostly-
// shared-across-users lookups, not per-round or per-user fan-out. See
// playerStatsResponse's doc comment for why win/loss isn't part of
// this: reconstructing it would mean fetching every past event's full
// pairings board round by round, which for a long history is on the
// order of hundreds of extra BCP requests just for one stat — decided
// against, in keeping with this service's "fetch only what's needed"
// rule.
type StatsHandler struct {
	store  userStore
	client bcpClient
}

// NewStatsHandler builds a StatsHandler.
func NewStatsHandler(store userStore, client bcpClient) *StatsHandler {
	return &StatsHandler{store: store, client: client}
}

// Register wires this handler's routes onto e. PlayerStats sits behind
// the same requireApprovedUser gate as Stats (this whole service is
// meant to be behind sign-in *and* approval — see BCPHandler.Register)
// but, unlike Stats, isn't scoped to the caller's own linked profile: it
// takes whatever BCP account id the frontend already has in hand for
// some other player (roster, pairings, placings — anywhere a name
// already carries a bcpUserId) and looks up that player's own summary
// instead of the caller's.
func (h *StatsHandler) Register(e *echo.Echo) {
	e.GET("/api/me/stats", h.Stats)
	e.GET("/api/players/:bcpUserId/stats", h.PlayerStats)
}

// Stats is GET /api/me/stats: the signed-in account's player-stats
// summary.
func (h *StatsHandler) Stats(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	if u.BcpUserID == "" {
		return c.JSON(http.StatusOK, emptyPlayerStatsResponse(false))
	}

	resp, err := h.statsForBcpUser(c.Request().Context(), u.BcpUserID)
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, resp)
}

// PlayerStats is GET /api/players/:bcpUserId/stats: the same summary as
// Stats, but for an arbitrary already-known BCP account id rather than
// the caller's own linked profile.
func (h *StatsHandler) PlayerStats(c echo.Context) error {
	if _, err := requireApprovedUser(c, h.store); err != nil {
		return err
	}
	bcpUserID := c.Param("bcpUserId")
	if bcpUserID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "bcpUserId is required"})
	}

	resp, err := h.statsForBcpUser(c.Request().Context(), bcpUserID)
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, resp)
}

// statsForBcpUser fetches and aggregates one BCP account's placing
// history into a playerStatsResponse — the shared body of both Stats
// and PlayerStats above, which differ only in where bcpUserID comes
// from (the caller's own linked profile vs. a path param).
func (h *StatsHandler) statsForBcpUser(ctx context.Context, bcpUserID string) (playerStatsResponse, error) {
	rawHistory, err := h.client.FetchPlacingHistory(ctx, bcpUserID)
	if err != nil {
		return playerStatsResponse{}, err
	}
	history := canonicalPlacingPerEvent(ctx, h.client, rawHistory)
	infos := eventInfoByID(ctx, h.client, history)

	resp := computePlayerStats(history, infos)

	if len(history) > 0 {
		// history is sorted most-recent-first (FetchPlacingHistory's own
		// contract, preserved by canonicalPlacingPerEvent).
		resp.MostRecentEventID = history[0].EventID
	}

	return resp, nil
}
