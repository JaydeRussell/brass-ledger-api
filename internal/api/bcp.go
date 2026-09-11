// Package api holds this service's HTTP handlers.
package api

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
)

// BCPHandler wires up every BCP-backed endpoint the frontend uses —
// this is the "heavy lifting" that used to happen in every browser tab
// (fetching from BCP, caching, rate limiting) moved here so it happens
// once, shared across every user of this app.
type BCPHandler struct {
	client bcpClient
}

// NewBCPHandler builds a BCPHandler.
func NewBCPHandler(client bcpClient) *BCPHandler {
	return &BCPHandler{client: client}
}

// Register wires this handler's routes onto e — most behind
// requireApproved (the whole app is meant to be behind sign-in *and*
// approval, not just the account-specific features elsewhere in this
// package), except Players, which only takes requireSession. That one
// exception is deliberate: the frontend's BCP-profile-linking flow
// (`/welcome`'s roster picker — see brass-ledger-web's
// BcpProfileLinker) calls this route to let a brand-new, still-pending
// account find and pick itself off a roster it already knows it's on
// — onboarding shouldn't be stuck waiting on approval just to reach the
// step where an admin would actually decide whether to approve it.
// Every other route stays behind full approval.
func (h *BCPHandler) Register(e *echo.Echo, requireApproved, requireSession echo.MiddlewareFunc) {
	e.GET("/api/events/:id", h.EventInfo, requireApproved)
	e.GET("/api/events/:id/players", h.Players, requireSession)
	e.GET("/api/events/:id/pairings", h.Pairings, requireApproved)
	e.GET("/api/events/:id/placings", h.Placings, requireApproved)
	e.GET("/api/itc/leagues/event/:eventId", h.ItcLeagueID, requireApproved)
	e.GET("/api/itc/rankings", h.ItcRanking, requireApproved)
}

// EventInfo is GET /api/events/:id.
func (h *BCPHandler) EventInfo(c echo.Context) error {
	info, err := h.client.FetchEventInfo(c.Request().Context(), c.Param("id"))
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, info)
}

// Players is GET /api/events/:id/players.
func (h *BCPHandler) Players(c echo.Context) error {
	players, err := h.client.FetchPlayers(c.Request().Context(), c.Param("id"))
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, players)
}

// Pairings is GET /api/events/:id/pairings. ?refresh=true forces a real
// BCP check instead of serving the normal cache — the frontend's
// RoundBoard "check for updated pairings" button (see
// Client.InvalidateRoundPairings' doc comment, and CLAUDE.md's no-polling
// rule for why this is opt-in per request rather than automatic). Same
// ?refresh=true convention as GET /api/me/events.
func (h *BCPHandler) Pairings(c echo.Context) error {
	pairingType := c.QueryParam("type")
	if pairingType != "Pairing" && pairingType != "TeamPairing" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": `"type" query param must be "Pairing" or "TeamPairing"`,
		})
	}
	round, err := strconv.Atoi(c.QueryParam("round"))
	if err != nil || round < 1 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": `"round" query param must be a positive integer`,
		})
	}

	if c.QueryParam("refresh") == "true" {
		h.client.InvalidateRoundPairings(c.Param("id"), pairingType, round)
	}

	records, err := h.client.FetchRoundPairings(c.Request().Context(), c.Param("id"), pairingType, round)
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, records)
}

// Placings is GET /api/events/:id/placings. ?refresh=true forces a real
// BCP check — see Pairings' doc comment above, same convention.
func (h *BCPHandler) Placings(c echo.Context) error {
	teamEvent := c.QueryParam("team") == "true"

	if c.QueryParam("refresh") == "true" {
		h.client.InvalidatePlacings(c.Param("id"), teamEvent)
	}

	entries, err := h.client.FetchPlacings(c.Request().Context(), c.Param("id"), teamEvent)
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, entries)
}

// ItcLeagueID is GET /api/itc/leagues/event/:eventId — resolves the
// current flagship ITC league among this specific event's own known
// leagues (EventInfo.LeagueIDs), not a game-system-wide search — see
// FetchCurrentItcLeagueIDForEvent's doc comment for why that search
// stopped being reliable. Costs no extra BCP request beyond the
// already-cached FetchEventInfo lookup every event page already makes.
func (h *BCPHandler) ItcLeagueID(c echo.Context) error {
	info, err := h.client.FetchEventInfo(c.Request().Context(), c.Param("eventId"))
	if err != nil {
		return bcpError(c, err)
	}
	leagueID, err := h.client.FetchCurrentItcLeagueIDForEvent(c.Request().Context(), info.LeagueIDs)
	if err != nil {
		return bcpError(c, err)
	}
	if leagueID == "" {
		return c.JSON(http.StatusOK, map[string]any{"leagueId": nil})
	}
	return c.JSON(http.StatusOK, map[string]any{"leagueId": leagueID})
}

// ItcRanking is GET /api/itc/rankings.
func (h *BCPHandler) ItcRanking(c echo.Context) error {
	leagueID := c.QueryParam("leagueId")
	bcpUserID := c.QueryParam("userId")
	if leagueID == "" || bcpUserID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": `"leagueId" and "userId" query params are required`,
		})
	}

	ranking, err := h.client.FetchItcRanking(c.Request().Context(), leagueID, bcpUserID)
	if err != nil {
		return bcpError(c, err)
	}
	if ranking == nil {
		return c.JSON(http.StatusOK, nil)
	}
	return c.JSON(http.StatusOK, ranking)
}

// bcpError maps an upstream BCP failure to a 502 (this service is a
// working proxy, but the thing it depends on failed) rather than a 500
// (which would suggest a bug in this service itself).
func bcpError(c echo.Context, err error) error {
	return c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
}
