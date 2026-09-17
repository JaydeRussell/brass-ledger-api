package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// friendRequestResponse is one entry in GET /api/friends/requests —
// only the requester's identity (name is all a recipient needs to
// decide whether to accept), not the recipient's own, since every
// caller of this route is asking "who's asking to friend *me*."
type friendRequestResponse struct {
	ID          int64  `json:"id"`
	RequesterID int64  `json:"requesterId"`
	Name        string `json:"name"`
	CreatedAt   string `json:"createdAt"`
}

func toFriendRequestResponses(requests []user.FriendRequest) []friendRequestResponse {
	resp := make([]friendRequestResponse, len(requests))
	for i, r := range requests {
		resp[i] = friendRequestResponse{
			ID:          r.ID,
			RequesterID: r.RequesterID,
			Name:        r.RequesterName,
			CreatedAt:   r.CreatedAt.Format(time.RFC3339),
		}
	}
	return resp
}

// friendResponse is one entry in GET /api/friends.
type friendResponse struct {
	UserID    int64  `json:"userId"`
	Name      string `json:"name"`
	BcpUserID string `json:"bcpUserId,omitempty"`
}

func toFriendResponses(friends []user.Friend) []friendResponse {
	resp := make([]friendResponse, len(friends))
	for i, f := range friends {
		resp[i] = friendResponse{UserID: f.UserID, Name: f.Name, BcpUserID: f.BcpUserID}
	}
	return resp
}

// sendFriendRequestBody is the body for POST /api/friends/requests.
type sendFriendRequestBody struct {
	RecipientBcpUserID string `json:"recipientBcpUserId"`
}

// FriendsHandler wires up mutual friending: sending/accepting/declining
// requests, listing friends, unfriending, and (the payoff) a friend's
// own upcoming events. See migration 0014's friend_requests table and
// internal/user/friends.go for the storage layer this wraps.
//
// Discovery is deliberately *not* a route here: there's no
// "search accounts" endpoint anywhere in this app (see
// internal/user.GetUserByBcpUserID's own doc comment) — sending a
// request always starts from a bcpUserId the caller already has in
// hand, typically from visiting someone's public dossier page (see
// brass-ledger-web's app/dossier/[bcpUserId]/page.tsx).
type FriendsHandler struct {
	store  userStore
	client bcpClient
}

// NewFriendsHandler builds a FriendsHandler.
func NewFriendsHandler(store userStore, client bcpClient) *FriendsHandler {
	return &FriendsHandler{store: store, client: client}
}

// Register wires this handler's routes onto e. Every route requires an
// approved session — friending is account-graph data, same bar as the
// rest of this app's real features.
func (h *FriendsHandler) Register(e *echo.Echo) {
	e.POST("/api/friends/requests", h.SendRequest)
	e.GET("/api/friends/requests", h.ListIncomingRequests)
	e.POST("/api/friends/requests/:id/accept", h.AcceptRequest)
	e.POST("/api/friends/requests/:id/decline", h.DeclineRequest)
	e.GET("/api/friends", h.ListFriends)
	e.DELETE("/api/friends/:id", h.RemoveFriend)
	e.GET("/api/friends/:bcpUserId/events", h.Events)
}

// SendRequest is POST /api/friends/requests: sends a pending friend
// request to whoever has linked recipientBcpUserId, resolved the same
// way DossierHandler resolves a bcpUserId to an account.
func (h *FriendsHandler) SendRequest(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}

	var req sendFriendRequestBody
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.RecipientBcpUserID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "recipientBcpUserId is required"})
	}

	ctx := c.Request().Context()
	recipient, err := h.store.GetUserByBcpUserID(ctx, req.RecipientBcpUserID)
	if err != nil {
		if errors.Is(err, user.ErrUserNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "no account found for that player"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if recipient.ID == u.ID {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "can't send a friend request to yourself"})
	}

	fr, err := h.store.SendFriendRequest(ctx, u.ID, recipient.ID)
	if err != nil {
		if errors.Is(err, user.ErrFriendRequestAlreadyExists) {
			return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"id": fr.ID, "status": fr.Status})
}

// ListIncomingRequests is GET /api/friends/requests: the signed-in
// account's own pending incoming requests.
func (h *FriendsHandler) ListIncomingRequests(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	requests, err := h.store.ListIncomingFriendRequests(c.Request().Context(), u.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, toFriendRequestResponses(requests))
}

// parseFriendRequestIDParam parses the :id path param shared by
// AcceptRequest/DeclineRequest.
func parseFriendRequestIDParam(c echo.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "id must be a positive integer"})
	}
	return id, nil
}

// AcceptRequest is POST /api/friends/requests/:id/accept.
func (h *FriendsHandler) AcceptRequest(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	id, err := parseFriendRequestIDParam(c)
	if err != nil {
		return err
	}
	if err := h.store.AcceptFriendRequest(c.Request().Context(), id, u.ID); err != nil {
		if errors.Is(err, user.ErrFriendRequestNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// DeclineRequest is POST /api/friends/requests/:id/decline.
func (h *FriendsHandler) DeclineRequest(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	id, err := parseFriendRequestIDParam(c)
	if err != nil {
		return err
	}
	if err := h.store.DeclineFriendRequest(c.Request().Context(), id, u.ID); err != nil {
		if errors.Is(err, user.ErrFriendRequestNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// ListFriends is GET /api/friends: the signed-in account's own accepted
// friends.
func (h *FriendsHandler) ListFriends(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	friends, err := h.store.ListFriends(c.Request().Context(), u.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, toFriendResponses(friends))
}

// RemoveFriend is DELETE /api/friends/:id — :id is the *other* account's
// internal user id (see friendResponse.UserID from ListFriends), not a
// friend_requests row id.
func (h *FriendsHandler) RemoveFriend(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	friendUserID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || friendUserID < 1 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id must be a positive integer"})
	}
	if err := h.store.RemoveFriend(c.Request().Context(), u.ID, friendUserID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// Events is GET /api/friends/:bcpUserId/events: the same
// past/present/future classification GET /api/me/events gives the
// signed-in account about themselves (see me.go's classifyMyEvents),
// but for a friend's linked BCP profile instead — the actual payoff of
// friending someone: "which events are we both going to." Gated on an
// accepted friendship between the caller and whichever account has
// linked bcpUserId; returns 404 (not 403) for "not your friend" so this
// can't be used to probe which bcpUserIds have a linked, friendable
// account at all versus which ones are just someone else's friend.
func (h *FriendsHandler) Events(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	bcpUserID := c.Param("bcpUserId")
	if bcpUserID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "bcpUserId is required"})
	}

	ctx := c.Request().Context()
	friendAccount, err := h.store.GetUserByBcpUserID(ctx, bcpUserID)
	if err != nil {
		if errors.Is(err, user.ErrUserNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	areFriends, err := h.store.AreFriends(ctx, u.ID, friendAccount.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if !areFriends {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}

	past, present, future, err := classifyMyEvents(ctx, h.client, bcpUserID, false)
	if err != nil {
		return bcpError(c, err)
	}
	resp := emptyMyEventsResponse(true)
	resp.Past, resp.Present, resp.Future = past, present, future
	return c.JSON(http.StatusOK, resp)
}
