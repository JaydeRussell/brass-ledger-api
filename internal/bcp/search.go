package bcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// --- Event search -------------------------------------------------------

// warhammer40kGameSystemID is BCP's id for Warhammer 40,000.
const warhammer40kGameSystemID = "WGMSzfKFYA"

const (
	// The window covers events running now and ones coming up, which is
	// what someone looking to spectate or join is after.
	searchWindowBefore = 7 * 24 * time.Hour
	searchWindowAfter  = 60 * 24 * time.Hour
	searchLimit        = 20
	// Event listings change when an organiser creates or edits an event,
	// not minute to minute.
	searchTTL = 10 * time.Minute
	// SearchMinQueryLength is the shortest query worth sending to BCP.
	SearchMinQueryLength = 3
)

// EventSearchResult is one event in a name search.
type EventSearchResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	StartDate   string `json:"startDate,omitempty"`
	EndDate     string `json:"endDate,omitempty"`
	Location    string `json:"location,omitempty"`
	PlayerCount *int   `json:"playerCount,omitempty"`
	TeamEvent   bool   `json:"teamEvent"`
	Started     bool   `json:"started"`
	Ended       bool   `json:"ended"`
}

type bcpEventListResponse struct {
	Data []struct {
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
		TeamEvent        bool   `json:"teamEvent"`
		Started          bool   `json:"started"`
		Ended            bool   `json:"ended"`
	} `json:"data"`
}

// NormalizeSearchQuery is the form BCP matches on: it only finds names
// when the query is lowercase, and matches it anywhere in the name.
func NormalizeSearchQuery(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// SearchEvents returns 40k events whose name contains query, from a week
// ago to two months ahead. query must already be normalized.
func (c *Client) SearchEvents(ctx context.Context, query string) ([]EventSearchResult, error) {
	return c.eventSearch.Get(ctx, query)
}

func (c *Client) searchEventsUncached(ctx context.Context, query string) ([]EventSearchResult, error) {
	now := time.Now().UTC()
	params := url.Values{}
	params.Set("searchString", query)
	params.Set("gameSystemId", warhammer40kGameSystemID)
	params.Set("startDate", now.Add(-searchWindowBefore).Format("2006-01-02")+"T00:00:00Z")
	params.Set("endDate", now.Add(searchWindowAfter).Format("2006-01-02")+"T23:59:59Z")
	params.Set("sortKey", "eventDate")
	params.Set("sortAscending", "true")
	params.Set("limit", fmt.Sprint(searchLimit))

	var body bcpEventListResponse
	if err := c.get(ctx, c.apiBaseV1+"/events?"+params.Encode(), &body); err != nil {
		return nil, err
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
			ID:          e.ID,
			Name:        e.Name,
			StartDate:   e.EventDate,
			EndDate:     e.EventEndDate,
			Location:    location,
			PlayerCount: e.TotalPlayers,
			TeamEvent:   e.TeamEvent,
			Started:     e.Started,
			Ended:       e.Ended,
		})
	}
	return results, nil
}
