package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
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
// back full RFC3339 ("2024-01-01T00:00:00.000Z") — see internal/bcp's
// events_test.go and history_test.go fixtures for both. Tried in order;
// the first that parses wins.
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
//
// UpcomingFetchedAt is when the Present/Future sections were last
// actually checked against BCP (RFC3339, omitted if never fetched this
// process's lifetime) — the registration list they're built from can go
// stale in a way only the signed-in user would know to ask about
// (registering for something new, or an event starting), so this is
// surfaced to the frontend as a "last updated" indicator alongside the
// ?refresh=true param below. Past isn't covered by this: an
// already-concluded event's placing never changes, so it has no
// meaningful staleness to report.
type myEventsResponse struct {
	Linked            bool      `json:"linked"`
	Past              []myEvent `json:"past"`
	Present           []myEvent `json:"present"`
	Future            []myEvent `json:"future"`
	UpcomingFetchedAt string    `json:"upcomingFetchedAt,omitempty"`
}

func emptyMyEventsResponse(linked bool) myEventsResponse {
	return myEventsResponse{
		Linked:  linked,
		Past:    []myEvent{},
		Present: []myEvent{},
		Future:  []myEvent{},
	}
}

// MeHandler wires up the signed-in user's own BCP-profile link and "my
// events" (past/present/future) endpoints — see the design note in
// internal/bcp/history.go's "Per-user event history" section for why
// classification needs two BCP endpoints plus a per-event fallback
// call, and CLAUDE.md's scope note: this only ever displays BCP's own
// already-published data, same as everything else in this service.
type MeHandler struct {
	store  userStore
	client bcpClient
}

// NewMeHandler builds a MeHandler.
func NewMeHandler(store userStore, client bcpClient) *MeHandler {
	return &MeHandler{store: store, client: client}
}

// Register wires this handler's routes onto e.
func (h *MeHandler) Register(e *echo.Echo) {
	e.POST("/api/me/bcp-profile", h.SetBcpProfile)
	e.GET("/api/me/events", h.Events)
}

// SetBcpProfile is POST /api/me/bcp-profile: links (or unlinks) a BCP
// profile to the signed-in account.
func (h *MeHandler) SetBcpProfile(c echo.Context) error {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}
	u, err := h.store.GetUserBySession(c.Request().Context(), cookie.Value)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}

	var req bcpProfileRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	bcpUserID := strings.TrimSpace(req.BcpUserID)

	if err := h.store.SetBcpUserID(c.Request().Context(), u.ID, bcpUserID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]any{"bcpUserId": bcpUserID})
}

// Events is GET /api/me/events: the signed-in account's BCP events,
// classified into past/present/future.
func (h *MeHandler) Events(c echo.Context) error {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}
	u, err := h.store.GetUserBySession(c.Request().Context(), cookie.Value)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}

	if u.BcpUserID == "" {
		return c.JSON(http.StatusOK, emptyMyEventsResponse(false))
	}

	ctx := c.Request().Context()

	// An explicit "check again now" request — see
	// bcp.Cache.Invalidate's doc comment for why this is scoped to
	// just the registration list (and, below, each not-yet-concluded
	// event's own info) rather than also forcing FetchPlacingHistory
	// to bypass its cache: an already-concluded event's placing can
	// never change, so there's nothing there a refresh could
	// meaningfully improve — only Present/Future can go stale in a
	// way the user would know to ask about.
	refresh := c.QueryParam("refresh") == "true"
	past, present, future, err := classifyMyEvents(ctx, h.client, u.BcpUserID, refresh)
	if err != nil {
		return bcpError(c, err)
	}

	resp := emptyMyEventsResponse(true)
	resp.Past, resp.Present, resp.Future = past, present, future

	if fetchedAt, ok := h.client.PlayerEventHistoryFetchedAt(u.BcpUserID); ok {
		resp.UpcomingFetchedAt = fetchedAt.Format(time.RFC3339)
	}

	return c.JSON(http.StatusOK, resp)
}

// classifyMyEvents fetches bcpUserID's placing history and registration
// list and classifies them into past/present/future — factored out of
// MeHandler.Events as its own fetch/classify step, separate from
// building the HTTP response around it. refresh mirrors
// MeHandler.Events' ?refresh=true (see its doc comment above for why
// this bypasses the registration-list/per-event cache but never
// FetchPlacingHistory).
//
// The three return slices start non-nil (empty, not nil) rather than as
// the named-return zero value — a section that ends up with zero
// entries (e.g. no events currently Present) must still encode as JSON
// `[]`, not `null`: the frontend spreads it directly
// (`[...events.present, ...events.future]`, see app/calendar/page.tsx),
// which throws on null.
func classifyMyEvents(ctx context.Context, client bcpClient, bcpUserID string, refresh bool) (past, present, future []myEvent, err error) {
	past, present, future = []myEvent{}, []myEvent{}, []myEvent{}

	if refresh {
		client.InvalidatePlayerEventHistory(bcpUserID)
	}

	placingHistory, err := client.FetchPlacingHistory(ctx, bcpUserID)
	if err != nil {
		return nil, nil, nil, err
	}
	registrations, err := client.FetchPlayerEventHistory(ctx, bcpUserID)
	if err != nil {
		return nil, nil, nil, err
	}

	concluded := make(map[string]bool, len(placingHistory))
	for _, p := range placingHistory {
		concluded[p.EventID] = true
		past = append(past, myEvent{
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
	// MeHandler above) — each needs its own FetchEventInfo call to learn
	// Started/Ended, since the registration list alone doesn't carry
	// dates.
	for _, r := range registrations {
		if concluded[r.EventID] {
			continue
		}
		if refresh {
			client.InvalidateEventInfo(r.EventID)
		}
		info, err := client.FetchEventInfo(ctx, r.EventID)
		if err != nil {
			// One event's metadata failing to load shouldn't take down
			// the whole list — skip just that event.
			continue
		}
		ev := myEvent{EventID: info.ID, EventName: info.Name, StartDate: info.StartDate, EndDate: info.EndDate}
		switch {
		case info.Started && !info.Ended && !isStaleEvent(info.EndDate):
			present = append(present, ev)
		case !info.Started:
			future = append(future, ev)
		default:
			// Either started and ended, or started-and-unended-but-
			// stale (see isStaleEvent) — either way BCP hasn't
			// published this user's placing yet (results still being
			// finalized, or the organizer never will), so the closest
			// bucket is Past even without placing details.
			past = append(past, ev)
		}
	}

	return past, present, future, nil
}
