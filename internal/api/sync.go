package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// followResponse is one entry in GET/POST /api/me/events/:eventId/follows
// — the wire shape mirrors the frontend's own Followed union (see
// brass-ledger-web's app/page.tsx) directly enough that the client
// doesn't need to reshape it.
type followResponse struct {
	Kind  string `json:"kind"`
	RefID string `json:"refId"`
	Label string `json:"label"`
}

// addFollowRequest is the body for POST /api/me/events/:eventId/follows.
type addFollowRequest struct {
	Kind  string `json:"kind"`
	RefID string `json:"refId"`
	Label string `json:"label"`
}

// followCountsResponse is GET /api/events/:id/follow-counts' body: how
// many distinct accounts follow each team/player within that event,
// keyed exactly like the frontend's own followedKey (see
// brass-ledger-web's app/lib/follows.ts) — "team:<teamPlayerId>" or
// "player:<playerId>" — so the frontend can look a count up directly
// without reshaping this response first.
type followCountsResponse map[string]int

func toFollowCountsResponse(counts []user.FollowCount) followCountsResponse {
	resp := make(followCountsResponse, len(counts))
	for _, c := range counts {
		resp[c.Kind+":"+c.RefID] = c.Count
	}
	return resp
}

// recentEventResponse is one entry in GET /api/me/recent-events.
type recentEventResponse struct {
	EventID      string `json:"eventId"`
	EventName    string `json:"eventName"`
	TeamEvent    bool   `json:"teamEvent"`
	LastViewedAt int64  `json:"lastViewedAt"` // unix millis, matching recentEvents.ts's Date.now()
}

// recordRecentEventRequest is the body for POST /api/me/recent-events.
type recordRecentEventRequest struct {
	EventID   string `json:"eventId"`
	EventName string `json:"eventName"`
	TeamEvent bool   `json:"teamEvent"`
}

// roundNoteResponse is GET /api/me/events/:eventId/rounds/:round/note's body.
type roundNoteResponse struct {
	Note string `json:"note"`
}

// setRoundNoteRequest is the body for PUT /api/me/events/:eventId/rounds/:round/note.
type setRoundNoteRequest struct {
	Note string `json:"note"`
}

// SyncHandler wires up cross-device sync for the two pieces of state
// that used to live only in per-browser localStorage: per-event follows
// (teams/players a user is tracking — see app/page.tsx's
// `following`/followingKey) and the global recently-viewed-events list
// (app/lib/recentEvents.ts). Every route here is session-gated the same
// way MeHandler's routes are — a signed-out request gets a 401, never a
// fallback to some anonymous/shared state.
//
// Deliberately action-shaped (POST to add a follow, DELETE to remove
// one) rather than "replace the whole list" — the frontend's localStorage
// version read-modified-wrote one big array, but that's a bad fit for a
// server multiple tabs/devices can hit concurrently: two devices each
// replacing the whole list would let one silently clobber the other's
// most recent change. Adding/removing one entry at a time avoids that.
type SyncHandler struct {
	store userStore
}

// NewSyncHandler builds a SyncHandler.
func NewSyncHandler(store userStore) *SyncHandler {
	return &SyncHandler{store: store}
}

// Register wires this handler's routes onto e.
func (h *SyncHandler) Register(e *echo.Echo) {
	e.GET("/api/me/events/:eventId/follows", h.ListFollows)
	e.POST("/api/me/events/:eventId/follows", h.AddFollow)
	e.DELETE("/api/me/events/:eventId/follows/:kind/:refId", h.RemoveFollow)
	e.GET("/api/events/:id/follow-counts", h.FollowCounts)
	e.GET("/api/me/recent-events", h.ListRecentEvents)
	e.POST("/api/me/recent-events", h.RecordRecentEvent)
	e.GET("/api/me/events/:eventId/rounds/:round/note", h.GetRoundNote)
	e.PUT("/api/me/events/:eventId/rounds/:round/note", h.SetRoundNote)
}

// ListFollows is GET /api/me/events/:eventId/follows.
func (h *SyncHandler) ListFollows(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	eventID := c.Param("eventId")

	follows, err := h.store.ListFollows(c.Request().Context(), u.ID, eventID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, toFollowResponses(follows))
}

// AddFollow is POST /api/me/events/:eventId/follows.
func (h *SyncHandler) AddFollow(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	eventID := c.Param("eventId")

	var req addFollowRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	kind := strings.TrimSpace(req.Kind)
	refID := strings.TrimSpace(req.RefID)
	if (kind != "team" && kind != "player") || refID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "kind must be \"team\" or \"player\", and refId is required"})
	}

	if err := h.store.AddFollow(c.Request().Context(), u.ID, eventID, kind, refID, req.Label); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// RemoveFollow is DELETE /api/me/events/:eventId/follows/:kind/:refId.
func (h *SyncHandler) RemoveFollow(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	eventID := c.Param("eventId")
	kind := c.Param("kind")
	refID := c.Param("refId")

	if err := h.store.RemoveFollow(c.Request().Context(), u.ID, eventID, kind, refID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// FollowCounts is GET /api/events/:id/follow-counts: for every
// team/player anyone follows within this event, how many distinct
// accounts follow it — social proof ("6 people tracking"), not tied to
// any one caller's own follows. Same requireApprovedUser gate as every
// other event-scoped route in this app (see BCPHandler.Register) —
// there's no reason for this one aggregate to be reachable by a
// signed-out visitor when the roster/pairings data it's counting
// against isn't.
func (h *SyncHandler) FollowCounts(c echo.Context) error {
	if _, err := requireApprovedUser(c, h.store); err != nil {
		return err
	}
	eventID := c.Param("id")

	counts, err := h.store.CountFollows(c.Request().Context(), eventID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, toFollowCountsResponse(counts))
}

// ListRecentEvents is GET /api/me/recent-events.
func (h *SyncHandler) ListRecentEvents(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}

	events, err := h.store.ListRecentEvents(c.Request().Context(), u.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	resp := make([]recentEventResponse, len(events))
	for i, ev := range events {
		resp[i] = recentEventResponse{
			EventID:      ev.EventID,
			EventName:    ev.EventName,
			TeamEvent:    ev.TeamEvent,
			LastViewedAt: ev.LastViewedAt.UnixMilli(),
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// RecordRecentEvent is POST /api/me/recent-events.
func (h *SyncHandler) RecordRecentEvent(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}

	var req recordRecentEventRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	eventID := strings.TrimSpace(req.EventID)
	if eventID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "eventId is required"})
	}

	if err := h.store.RecordRecentEvent(c.Request().Context(), u.ID, eventID, req.EventName, req.TeamEvent); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// parseRoundParam parses the :round path param as a positive round
// number — shared by GetRoundNote/SetRoundNote below.
func parseRoundParam(c echo.Context) (int, error) {
	round, err := strconv.Atoi(c.Param("round"))
	if err != nil || round < 1 {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "round must be a positive integer"})
	}
	return round, nil
}

// GetRoundNote is GET /api/me/events/:eventId/rounds/:round/note — a
// signed-in account's own private note for that round, "" if they've
// never saved one.
func (h *SyncHandler) GetRoundNote(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	round, err := parseRoundParam(c)
	if err != nil {
		return err
	}

	note, err := h.store.GetRoundNote(c.Request().Context(), u.ID, c.Param("eventId"), round)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, roundNoteResponse{Note: note})
}

// SetRoundNote is PUT /api/me/events/:eventId/rounds/:round/note —
// saves (or, given an empty/whitespace-only note, clears) a signed-in
// account's private note for that round.
func (h *SyncHandler) SetRoundNote(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	round, err := parseRoundParam(c)
	if err != nil {
		return err
	}

	var req setRoundNoteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	if err := h.store.SetRoundNote(c.Request().Context(), u.ID, c.Param("eventId"), round, req.Note); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// requireUser is the same session-cookie-then-store-lookup check
// AuthHandler.Me does inline (auth.go) — factored out here for every
// handler that needs the user value directly rather than through
// RequireApproved's middleware form. Deliberately doesn't check
// Status: nothing currently calls this directly except
// requireApprovedUser below (every other handler in this package wants
// the approval check too) — kept separate anyway so that distinction
// stays explicit rather than buried in one do-everything function.
func requireUser(c echo.Context, store userStore) (user.User, error) {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return user.User{}, c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}
	u, err := store.GetUserBySession(c.Request().Context(), cookie.Value)
	if err != nil {
		return user.User{}, c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}
	return u, nil
}

// requireApprovedUser is requireUser plus Status == StatusApproved —
// what every handler in this file, StatsHandler, and MeHandler actually
// want (see RequireApproved in auth.go for the middleware equivalent,
// used where a handler doesn't need the user value itself).
func requireApprovedUser(c echo.Context, store userStore) (user.User, error) {
	u, err := requireUser(c, store)
	if err != nil {
		return user.User{}, err
	}
	if u.Status != user.StatusApproved {
		return user.User{}, c.JSON(http.StatusForbidden, map[string]string{
			"error":  "account not approved",
			"status": u.Status,
		})
	}
	return u, nil
}

func toFollowResponses(follows []user.Follow) []followResponse {
	resp := make([]followResponse, len(follows))
	for i, f := range follows {
		resp[i] = followResponse{Kind: f.Kind, RefID: f.RefID, Label: f.Label}
	}
	return resp
}
