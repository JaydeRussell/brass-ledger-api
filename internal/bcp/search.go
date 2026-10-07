package bcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// --- Event search -------------------------------------------------------

// warhammer40kGameSystemID is BCP's id for Warhammer 40,000.
const warhammer40kGameSystemID = "WGMSzfKFYA"

const (
	// The window covers events running now and ones coming up, which is
	// what someone looking to spectate or join is after. Two days back
	// catches a weekend event still underway.
	searchWindowBefore = 2 * 24 * time.Hour
	searchWindowAfter  = 60 * 24 * time.Hour
	// Results per page.
	searchPageSize = 25
	// Event listings change when an organiser creates or edits an event,
	// not minute to minute.
	searchTTL = 10 * time.Minute
	// SearchMinQueryLength is the shortest query worth sending to BCP.
	SearchMinQueryLength = 3
)

// SearchRadiiMiles are the distances a location search can cover.
var SearchRadiiMiles = []int{25, 50, 100, 250}

// EventSearchNear limits a search to events within RadiusMiles of a point.
type EventSearchNear struct {
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	RadiusMiles int     `json:"radiusMiles"`
}

// EventSearchParams filters an event search. Query must already be
// normalized (see NormalizeSearchQuery) and may be empty when Near is set.
// From and To are YYYY-MM-DD; an empty one falls back to the default
// window of two days ago to two months ahead.
type EventSearchParams struct {
	Query string           `json:"q,omitempty"`
	Near  *EventSearchNear `json:"near,omitempty"`
	From  string           `json:"from,omitempty"`
	To    string           `json:"to,omitempty"`
}

// EventSearchResult is one event in a name search.
type EventSearchResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	StartDate   string `json:"startDate,omitempty"`
	EndDate     string `json:"endDate,omitempty"`
	Location    string `json:"location,omitempty"`
	PlayerCount *int   `json:"playerCount,omitempty"`
	// Capacity is the number of tickets, for singles events with a limit.
	// A team event's tickets usually count teams while PlayerCount counts
	// players, so it's left out there rather than compared.
	Capacity *int `json:"capacity,omitempty"`
	// DistanceMiles is from the search's centre to the venue, rounded to
	// a whole mile; only set on a location search.
	DistanceMiles *int `json:"distanceMiles,omitempty"`
	TeamEvent     bool `json:"teamEvent"`
	Started       bool `json:"started"`
	Ended         bool `json:"ended"`
}

// EventSearchPage is one page of a name search, oldest first. NextCursor
// is empty on the last page.
type EventSearchPage struct {
	Results    []EventSearchResult `json:"results"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

type bcpEventListResponse struct {
	NextKey json.RawMessage `json:"nextKey"`
	Data    []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		EventDate        string `json:"eventDate"`
		EventEndDate     string `json:"eventEndDate"`
		LocationName     string `json:"locationName"`
		City             string `json:"city"`
		State            string `json:"state"`
		Country          string `json:"country"`
		FormattedAddress string `json:"formatted_address"`
		TotalPlayers     *int   `json:"totalPlayers"`
		NumTickets       int    `json:"numTickets"`
		// [longitude, latitude]
		Coordinate      []float64 `json:"coordinate"`
		HidePlayerCount bool      `json:"hidePlayerCount"`
		TeamEvent       bool      `json:"teamEvent"`
		Started         bool      `json:"started"`
		Ended           bool      `json:"ended"`
	} `json:"data"`
}

// NormalizeSearchQuery is the form BCP matches on: it only finds names
// when the query is lowercase, and matches it anywhere in the name.
func NormalizeSearchQuery(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// SearchEvents returns one page of 40k events matching p, oldest first.
// cursor is a previous page's NextCursor, or empty for the first page.
func (c *Client) SearchEvents(ctx context.Context, p EventSearchParams, cursor string) (EventSearchPage, error) {
	key, err := json.Marshal(p)
	if err != nil {
		return EventSearchPage{}, err
	}
	return c.eventSearch.Get(ctx, string(key)+"\n"+cursor)
}

// searchEventsByKey runs the search a SearchEvents cache key describes:
// the params as JSON, a newline, then the cursor.
func (c *Client) searchEventsByKey(ctx context.Context, key string) (EventSearchPage, error) {
	paramsJSON, cursor, _ := strings.Cut(key, "\n")
	var p EventSearchParams
	if err := json.Unmarshal([]byte(paramsJSON), &p); err != nil {
		return EventSearchPage{}, err
	}
	return c.searchEventsUncached(ctx, p, cursor)
}

func (c *Client) searchEventsUncached(ctx context.Context, p EventSearchParams, cursor string) (EventSearchPage, error) {
	now := time.Now().UTC()
	from, to := p.From, p.To
	if from == "" {
		from = now.Add(-searchWindowBefore).Format("2006-01-02")
	}
	if to == "" {
		to = now.Add(searchWindowAfter).Format("2006-01-02")
	}
	params := url.Values{}
	if p.Query != "" {
		params.Set("searchString", p.Query)
	}
	if p.Near != nil {
		loc, err := json.Marshal(map[string]any{
			"distance":     p.Near.RadiusMiles,
			"center":       map[string]float64{"lat": p.Near.Lat, "long": p.Near.Lon},
			"distanceType": "miles",
		})
		if err != nil {
			return EventSearchPage{}, err
		}
		params.Set("location", string(loc))
		params.Set("distanceType", "miles")
		// BCP ignores location unless online events are excluded.
		params.Set("excludeOnline", "true")
	}
	params.Set("gameSystemId", warhammer40kGameSystemID)
	params.Set("startDate", from+"T00:00:00Z")
	params.Set("endDate", to+"T23:59:59Z")
	params.Set("sortKey", "eventDate")
	params.Set("sortAscending", "true")
	params.Set("limit", fmt.Sprint(searchPageSize))
	if cursor != "" {
		params.Set("nextKey", cursor)
	}

	var body bcpEventListResponse
	if err := c.get(ctx, c.apiBaseV1+"/events?"+params.Encode(), &body); err != nil {
		return EventSearchPage{}, err
	}

	results := make([]EventSearchResult, 0, len(body.Data))
	for _, e := range body.Data {
		if e.ID == "" {
			continue
		}
		location := e.FormattedAddress
		if location == "" {
			location = strings.Join(nonEmpty(e.LocationName, e.City, e.State, e.Country), ", ")
		}
		results = append(results, EventSearchResult{
			ID:            e.ID,
			Name:          e.Name,
			StartDate:     e.EventDate,
			EndDate:       e.EventEndDate,
			Location:      location,
			PlayerCount:   playerCount(e.TotalPlayers, e.HidePlayerCount),
			Capacity:      capacity(e.NumTickets, e.TeamEvent, e.HidePlayerCount),
			DistanceMiles: distanceFrom(p.Near, e.Coordinate),
			TeamEvent:     e.TeamEvent,
			Started:       e.Started,
			Ended:         e.Ended,
		})
	}
	page := EventSearchPage{Results: results}
	// BCP can hand back a cursor on its last page that only leads to an
	// empty one, so a short page is the end too.
	if len(body.Data) == searchPageSize {
		next, err := decodeNextKey(body.NextKey)
		if err != nil {
			return EventSearchPage{}, err
		}
		page.NextCursor = next
	}
	return page, nil
}

// playerCount is nil when the organiser has hidden it on BCP.
func playerCount(total *int, hidden bool) *int {
	if hidden {
		return nil
	}
	return total
}

// capacity is a singles event's ticket limit, or nil when there's no limit
// (BCP sends 0), the count is hidden, or it's a team event.
func capacity(tickets int, teamEvent, hidden bool) *int {
	if tickets <= 0 || teamEvent || hidden {
		return nil
	}
	return &tickets
}

const earthRadiusMiles = 3958.8

// distanceFrom is the great-circle distance from a location search's centre
// to a venue at coord ([longitude, latitude]), or nil without either.
func distanceFrom(near *EventSearchNear, coord []float64) *int {
	if near == nil || len(coord) != 2 {
		return nil
	}
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	lat1, lat2 := rad(near.Lat), rad(coord[1])
	dLat, dLon := lat2-lat1, rad(coord[0]-near.Lon)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	miles := int(math.Round(2 * earthRadiusMiles * math.Asin(math.Sqrt(h))))
	return &miles
}
