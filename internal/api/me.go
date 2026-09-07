package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/teams-match-making-be/internal/bcp"
)

// staleEventAfter is how long past its listed end date an event that BCP
// still shows as "started, not ended" gets reclassified from Present to
// Past anyway. Some organizers never flip the "ended" switch on their own
// event even once it's clearly over, which would otherwise leave it
// stuck in "Ongoing" on the frontend's My Events page forever. A few
// days' grace avoids misclassifying an event that's still legitimately
// running a little long (finals dragging on, a rescheduled last round).
const staleEventAfter = 3 * 24 * time.Hour

// bcpDateLayouts are the two date shapes this service has actually seen
// from BCP: FetchEventInfo's Dates.Start/End come back as a bare date
// ("2026-01-01"), while FetchPlacingHistory's EventDate/EventEndDate come
// back full RFC3339 ("2024-01-01T00:00:00.000Z") — see client_test.go's
// fixtures for both. Tried in order; the first that parses wins.
var bcpDateLayouts = []string{time.RFC3339, "2006-01-02"}

// parseBCPDate parses whichever of bcpDateLayouts matches, or reports ok
// = false for an empty or unrecognized string — BCP not publishing a
// date for something isn't an error here, just a "can't tell" for
// whatever the caller was trying to decide (see isStaleEvent below).
func parseBCPDate(s string) (t time.Time, ok bool) {
	for _, layout := range bcpDateLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// isStaleEvent reports whether an event's listed end date is far enough
// in the past that it should be treated as concluded regardless of what
// BCP's own Started/Ended flags say — see staleEventAfter's doc comment.
func isStaleEvent(endDate string) bool {
	parsed, ok := parseBCPDate(endDate)
	if !ok {
		return false
	}
	return time.Since(parsed) > staleEventAfter
}

// bcpProfileRequest is the body for POST /api/me/bcp-profile. The
// frontend is responsible for pulling a bare BCP user id out of
// whatever the user pastes (a profile URL or a raw id) — see that
// repo's app/lib/myEvents.ts — so this route only ever sees the id
// itself.
type bcpProfileRequest struct {
	BcpUserID string `json:"bcpUserId"`
}

// myEvent is one event in a GET /api/me/events response. The same
// shape covers all three sections: Past entries additionally carry
// Placing/Points/Faction/Team (from BCP's placings-history endpoint);
// Present/Future entries leave those empty, since BCP hasn't published
// a placing for an event that hasn't concluded yet.
type myEvent struct {
	EventID   string   `json:"eventId"`
	EventName string   `json:"eventName"`
	StartDate string   `json:"startDate,omitempty"`
	EndDate   string   `json:"endDate,omitempty"`
	Placing   *int     `json:"placing,omitempty"`
	Points    *float64 `json:"points,omitempty"`
	Faction   string   `json:"faction,omitempty"`
	Team      string   `json:"team,omitempty"`
}

// myEventsResponse is GET /api/me/events' full body. Linked is false
// (with the three sections empty) for an account that hasn't pasted a
// BCP profile yet — a normal, expected state for a new account, not an
// error — so the frontend can tell "not linked" apart from "linked but
// no events yet".
type myEventsResponse struct {
	Linked  bool      `json:"linked"`
	Past    []myEvent `json:"past"`
	Present []myEvent `json:"present"`
	Future  []myEvent `json:"future"`
}

func emptyMyEventsResponse(linked bool) myEventsResponse {
	return myEventsResponse{
		Linked:  linked,
		Past:    []myEvent{},
		Present: []myEvent{},
		Future:  []myEvent{},
	}
}

// RegisterMeRoutes wires up the signed-in user's own BCP-profile link
// and "my events" (past/present/future) endpoints — see the design note
// in internal/bcp/client.go's "Per-user event history" section for why
// classification needs two BCP endpoints plus a per-event fallback
// call, and CLAUDE.md's scope note: this only ever displays BCP's own
// already-published data, same as everything else in this service.
func RegisterMeRoutes(e *echo.Echo, store userStore, bcpClient *bcp.Client) {
	e.POST("/api/me/bcp-profile", func(c echo.Context) error {
		cookie, err := c.Cookie(sessionCookieName)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		}
		u, err := store.GetUserBySession(c.Request().Context(), cookie.Value)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		}

		var req bcpProfileRequest
		if err := c.Bind(&req); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		}
		bcpUserID := strings.TrimSpace(req.BcpUserID)

		if err := store.SetBcpUserID(c.Request().Context(), u.ID, bcpUserID); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		return c.JSON(http.StatusOK, map[string]any{"bcpUserId": bcpUserID})
	})

	e.GET("/api/me/events", func(c echo.Context) error {
		cookie, err := c.Cookie(sessionCookieName)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		}
		u, err := store.GetUserBySession(c.Request().Context(), cookie.Value)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		}

		if u.BcpUserID == "" {
			return c.JSON(http.StatusOK, emptyMyEventsResponse(false))
		}

		ctx := c.Request().Context()

		placingHistory, err := bcpClient.FetchPlacingHistory(ctx, u.BcpUserID)
		if err != nil {
			return bcpError(c, err)
		}
		registrations, err := bcpClient.FetchPlayerEventHistory(ctx, u.BcpUserID)
		if err != nil {
			return bcpError(c, err)
		}

		resp := emptyMyEventsResponse(true)

		concluded := make(map[string]bool, len(placingHistory))
		for _, p := range placingHistory {
			concluded[p.EventID] = true
			resp.Past = append(resp.Past, myEvent{
				EventID:   p.EventID,
				EventName: p.EventName,
				StartDate: p.EventDate,
				EndDate:   p.EventEndDate,
				Placing:   p.Placing,
				Points:    p.Points,
				Faction:   p.Faction,
				Team:      p.Team,
			})
		}

		// Anything registered but not yet in the placings history is the
		// small number of current/upcoming events (see the design note on
		// RegisterMeRoutes above) — each needs its own FetchEventInfo call
		// to learn Started/Ended, since the registration list alone
		// doesn't carry dates.
		for _, r := range registrations {
			if concluded[r.EventID] {
				continue
			}
			info, err := bcpClient.FetchEventInfo(ctx, r.EventID)
			if err != nil {
				// One event's metadata failing to load shouldn't take down
				// the whole list — skip just that event.
				continue
			}
			ev := myEvent{EventID: info.ID, EventName: info.Name, StartDate: info.StartDate, EndDate: info.EndDate}
			switch {
			case info.Started && !info.Ended && !isStaleEvent(info.EndDate):
				resp.Present = append(resp.Present, ev)
			case !info.Started:
				resp.Future = append(resp.Future, ev)
			default:
				// Either started and ended, or started-and-unended-but-
				// stale (see isStaleEvent) — either way BCP hasn't
				// published this user's placing yet (results still being
				// finalized, or the organizer never will), so the closest
				// bucket is Past even without placing details.
				resp.Past = append(resp.Past, ev)
			}
		}

		return c.JSON(http.StatusOK, resp)
	})
}
