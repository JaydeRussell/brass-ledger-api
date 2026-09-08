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

// Register wires this handler's routes onto e.
func (h *BCPHandler) Register(e *echo.Echo) {
	e.GET("/api/events/:id", h.EventInfo)
	e.GET("/api/events/:id/players", h.Players)
	e.GET("/api/events/:id/pairings", h.Pairings)
	e.GET("/api/events/:id/placings", h.Placings)
	e.GET("/api/itc/leagues/:gameSystemId", h.ItcLeagueID)
	e.GET("/api/itc/rankings", h.ItcRanking)
}

func (h *BCPHandler) EventInfo(c echo.Context) error {
	info, err := h.client.FetchEventInfo(c.Request().Context(), c.Param("id"))
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, info)
}

func (h *BCPHandler) Players(c echo.Context) error {
	players, err := h.client.FetchPlayers(c.Request().Context(), c.Param("id"))
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, players)
}

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

	records, err := h.client.FetchRoundPairings(c.Request().Context(), c.Param("id"), pairingType, round)
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, records)
}

func (h *BCPHandler) Placings(c echo.Context) error {
	teamEvent := c.QueryParam("team") == "true"
	entries, err := h.client.FetchPlacings(c.Request().Context(), c.Param("id"), teamEvent)
	if err != nil {
		return bcpError(c, err)
	}
	return c.JSON(http.StatusOK, entries)
}

func (h *BCPHandler) ItcLeagueID(c echo.Context) error {
	leagueID, err := h.client.FetchCurrentItcLeagueID(c.Request().Context(), c.Param("gameSystemId"))
	if err != nil {
		return bcpError(c, err)
	}
	if leagueID == "" {
		return c.JSON(http.StatusOK, map[string]any{"leagueId": nil})
	}
	return c.JSON(http.StatusOK, map[string]any{"leagueId": leagueID})
}

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
