package bcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"
)

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

// InvalidatePlayerEventHistory forces the next FetchPlayerEventHistory
// call for this user to hit BCP for real — the registration list is the
// only signal source for "did I just sign up for something new," which
// the normal cache TTL can't detect on its own. See Cache.Invalidate's
// doc comment for why this is meant for an explicit "check again now"
// action, not routine use.
func (c *Client) InvalidatePlayerEventHistory(bcpUserID string) {
	c.playerEventHistory.Invalidate(bcpUserID)
}

// PlayerEventHistoryFetchedAt reports when this user's registration list
// was last actually fetched from BCP, or ok=false if it's never been
// fetched at all this process's lifetime. The "last updated" timestamp
// for the Ongoing/Future sections of My Events — Past isn't covered by
// this since an already-concluded event's placing never changes, so it
// has no meaningful staleness to report.
func (c *Client) PlayerEventHistoryFetchedAt(bcpUserID string) (time.Time, bool) {
	return c.playerEventHistory.FetchedAt(bcpUserID)
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
