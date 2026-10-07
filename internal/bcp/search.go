package bcp

import (
	"context"
	"encoding/json"
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

// SearchEvents returns one page of 40k events whose name contains query,
// from two days ago to two months ahead. query must already be
// normalized; cursor is a previous page's NextCursor, or empty for the
// first page.
func (c *Client) SearchEvents(ctx context.Context, query, cursor string) (EventSearchPage, error) {
	return c.eventSearch.Get(ctx, query+"\n"+cursor)
}

func splitSearchKey(key string) (query, cursor string) {
	query, cursor, _ = strings.Cut(key, "\n")
	return query, cursor
}

func (c *Client) searchEventsUncached(ctx context.Context, query, cursor string) (EventSearchPage, error) {
	now := time.Now().UTC()
	params := url.Values{}
	params.Set("searchString", query)
	params.Set("gameSystemId", warhammer40kGameSystemID)
	params.Set("startDate", now.Add(-searchWindowBefore).Format("2006-01-02")+"T00:00:00Z")
	params.Set("endDate", now.Add(searchWindowAfter).Format("2006-01-02")+"T23:59:59Z")
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
