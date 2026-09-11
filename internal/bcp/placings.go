package bcp

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// --- Placings -------------------------------------------------------------

type bcpPlacingRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"` // team events
	User struct {
		ID        string `json:"id"`
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
	durableKey := "placings:" + placingsKey(eventID, teamEvent)
	if c.durable != nil {
		var cached []PlacingEntry
		if found, err := c.durable.Get(ctx, durableKey, &cached); err == nil && found {
			return cached, nil
		}
	}

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
			ID:        r.ID,
			Name:      name,
			Placing:   r.Placing,
			Metrics:   metrics,
			BcpUserID: r.User.ID,
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

	// Standings only stop changing once the event itself has concluded —
	// same reasoning as fetchPlayersUncached/fetchRoundPairingsUncached
	// above.
	if c.durable != nil {
		if info, err := c.FetchEventInfo(ctx, eventID); err == nil && info.Ended {
			_ = c.durable.Set(ctx, durableKey, entries)
		}
	}

	return entries, nil
}

// FetchPlacings returns cached, rate-limited standings — whatever BCP
// has already computed and published, ranked by its own `placing`
// field. Empty until BCP has actually placed anyone, which typically
// means at least one round has finished.
func (c *Client) FetchPlacings(ctx context.Context, eventID string, teamEvent bool) ([]PlacingEntry, error) {
	return c.placings.Get(ctx, placingsKey(eventID, teamEvent))
}

// InvalidatePlacings forces the next FetchPlacings call for this event
// to hit BCP for real — the frontend's "check for updated placings"
// button (see CLAUDE.md's no-polling rule). A no-op for an event whose
// placings are already durably cached (an already-concluded event's
// standings never change) since fetchPlacingsUncached checks that
// before ever reaching this in-memory one.
func (c *Client) InvalidatePlacings(eventID string, teamEvent bool) {
	c.placings.Invalidate(placingsKey(eventID, teamEvent))
}
