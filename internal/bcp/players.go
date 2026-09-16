package bcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

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
// every entry in BCP's own `active` list is a real registrant — confirmed
// live against an event with zero submitted lists yet, where BCP's own
// roster page still lists all of them (as "not checked in") — so this no
// longer gates on having a submitted list (a previous version of this
// code did; that hid a real, still-forming roster for as long as no one
// had submitted a list yet, which is exactly the state a newly-opened
// event's Roster tab is in for a while). Each player's actual per-event
// tournament team name is resolved via teamPlayerId against the separate
// /teamplayers collection.
func playersDurableKey(eventID string) string { return "players:" + eventID }

func (c *Client) fetchPlayersUncached(ctx context.Context, eventID string) ([]Player, error) {
	if c.durable != nil {
		var cached []Player
		if found, err := c.durable.Get(ctx, playersDurableKey(eventID), &cached); err == nil && found {
			return cached, nil
		}
	}

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

		var disposition string
		if forceDispositions[r.SubFaction.Name] {
			disposition = r.SubFaction.Name
		}

		players = append(players, Player{
			ID:           r.ID,
			Name:         name,
			Faction:      faction,
			SubFaction:   r.SubFaction.Name,
			Disposition:  disposition,
			Team:         teamName,
			TeamPlayerID: r.TeamPlayerID,
			HomeClub:     r.Team.Name,
			List:         list,
			BcpUserID:    r.User.ID,
		})
	}

	// A roster only stops changing once the event itself has concluded —
	// FetchEventInfo is itself cached (in-memory, and durably once
	// ended), so this costs nothing extra once an event's info has been
	// fetched at all, which every real caller does before ever reaching
	// the Roster tab.
	if c.durable != nil {
		if info, err := c.FetchEventInfo(ctx, eventID); err == nil && info.Ended {
			_ = c.durable.Set(ctx, playersDurableKey(eventID), players)
		}
	}

	return players, nil
}

// FetchPlayers returns cached, rate-limited roster data for an event —
// every registered player, individual or team, regardless of whether
// they've submitted a list yet.
func (c *Client) FetchPlayers(ctx context.Context, eventID string) ([]Player, error) {
	return c.players.Get(ctx, eventID)
}
