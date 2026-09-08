package api

import (
	"net/http"
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
	e.GET("/api/me/recent-events", h.ListRecentEvents)
	e.POST("/api/me/recent-events", h.RecordRecentEvent)
}

// ListFollows is GET /api/me/events/:eventId/follows.
func (h *SyncHandler) ListFollows(c echo.Context) error {
	u, err := requireUser(c, h.store)
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
	u, err := requireUser(c, h.store)
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
	u, err := requireUser(c, h.store)
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

// ListRecentEvents is GET /api/me/recent-events.
func (h *SyncHandler) ListRecentEvents(c echo.Context) error {
	u, err := requireUser(c, h.store)
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
	u, err := requireUser(c, h.store)
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

// requireUser is the same session-cookie-then-store-lookup check
// MeHandler's methods each repeat inline; factored out here since this
// file adds five more routes needing it (me.go's two are left as-is
// rather than churning an already-tested file for a style-only change).
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

func toFollowResponses(follows []user.Follow) []followResponse {
	resp := make([]followResponse, len(follows))
	for i, f := range follows {
		resp[i] = followResponse{Kind: f.Kind, RefID: f.RefID, Label: f.Label}
	}
	return resp
}
