// Package api holds this service's HTTP handlers.
package api

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/teams-match-making-be/internal/bcp"
)

// RegisterBCPRoutes wires up every BCP-backed endpoint the frontend
// uses — this is the "heavy lifting" that used to happen in every
// browser tab (fetching from BCP, caching, rate limiting) moved here so
// it happens once, shared across every user of this app.
func RegisterBCPRoutes(e *echo.Echo, client *bcp.Client) {
	e.GET("/api/events/:id", func(c echo.Context) error {
		info, err := client.FetchEventInfo(c.Request().Context(), c.Param("id"))
		if err != nil {
			return bcpError(c, err)
		}
		return c.JSON(http.StatusOK, info)
	})

	e.GET("/api/events/:id/players", func(c echo.Context) error {
		players, err := client.FetchPlayers(c.Request().Context(), c.Param("id"))
		if err != nil {
			return bcpError(c, err)
		}
		return c.JSON(http.StatusOK, players)
	})

	e.GET("/api/events/:id/pairings", func(c echo.Context) error {
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

		records, err := client.FetchRoundPairings(c.Request().Context(), c.Param("id"), pairingType, round)
		if err != nil {
			return bcpError(c, err)
		}
		return c.JSON(http.StatusOK, records)
	})

	e.GET("/api/events/:id/placings", func(c echo.Context) error {
		teamEvent := c.QueryParam("team") == "true"
		entries, err := client.FetchPlacings(c.Request().Context(), c.Param("id"), teamEvent)
		if err != nil {
			return bcpError(c, err)
		}
		return c.JSON(http.StatusOK, entries)
	})

	e.GET("/api/itc/leagues/:gameSystemId", func(c echo.Context) error {
		leagueID, err := client.FetchCurrentItcLeagueID(c.Request().Context(), c.Param("gameSystemId"))
		if err != nil {
			return bcpError(c, err)
		}
		if leagueID == "" {
			return c.JSON(http.StatusOK, map[string]any{"leagueId": nil})
		}
		return c.JSON(http.StatusOK, map[string]any{"leagueId": leagueID})
	})

	e.GET("/api/itc/rankings", func(c echo.Context) error {
		leagueID := c.QueryParam("leagueId")
		bcpUserID := c.QueryParam("userId")
		if leagueID == "" || bcpUserID == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": `"leagueId" and "userId" query params are required`,
			})
		}

		ranking, err := client.FetchItcRanking(c.Request().Context(), leagueID, bcpUserID)
		if err != nil {
			return bcpError(c, err)
		}
		if ranking == nil {
			return c.JSON(http.StatusOK, nil)
		}
		return c.JSON(http.StatusOK, ranking)
	})
}

// bcpError maps an upstream BCP failure to a 502 (this service is a
// working proxy, but the thing it depends on failed) rather than a 500
// (which would suggest a bug in this service itself).
func bcpError(c echo.Context, err error) error {
	return c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
}
