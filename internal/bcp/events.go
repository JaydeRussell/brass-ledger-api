package bcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

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
		ID   string `json:"id"`
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

func eventInfoDurableKey(eventID string) string { return "event:" + eventID }

func (c *Client) fetchEventInfoUncached(ctx context.Context, eventID string) (EventInfo, error) {
	if c.durable != nil {
		var cached EventInfo
		if found, err := c.durable.Get(ctx, eventInfoDurableKey(eventID), &cached); err == nil && found {
			return cached, nil
		}
	}

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
	leagueIDs := make([]string, 0, len(body.Leagues))
	for _, l := range body.Leagues {
		if l.Name != "" {
			circuits = append(circuits, l.Name)
		}
		if l.ID != "" {
			leagueIDs = append(leagueIDs, l.ID)
		}
	}

	info := EventInfo{
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
		LeagueIDs:         leagueIDs,
	}

	// Only an already-concluded event's info is safe to persist forever —
	// Started/Ended/CurrentRound and everything else here can still
	// change for one that hasn't ended yet. A failed write just means
	// this gets asked of BCP again next time; not worth failing the
	// request over.
	if c.durable != nil && info.Ended {
		_ = c.durable.Set(ctx, eventInfoDurableKey(eventID), info)
	}

	return info, nil
}

// FetchEventInfo returns cached, rate-limited event metadata.
func (c *Client) FetchEventInfo(ctx context.Context, eventID string) (EventInfo, error) {
	return c.eventInfo.Get(ctx, eventID)
}

// InvalidateEventInfo forces the next FetchEventInfo call for this event
// to hit BCP for real — used alongside InvalidatePlayerEventHistory so a
// present/future event's own status (e.g. one that just started, or
// just concluded) is also rechecked on a manual "refresh my events"
// request, not just whether new registrations appeared. A no-op for an
// event whose info is already durably cached (an already-concluded
// event never needs rechecking at all) since fetchEventInfoUncached
// checks the durable cache before ever reaching this in-memory one.
func (c *Client) InvalidateEventInfo(eventID string) {
	c.eventInfo.Invalidate(eventID)
}
