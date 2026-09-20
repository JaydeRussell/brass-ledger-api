// Package api holds this service's HTTP handlers.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
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
	// The one route that knows for free whether its own answer can ever
	// change again.
	cacheFor(c, info.Ended)
	return c.JSON(http.StatusOK, info)
}

// Players is GET /api/events/:id/players.
func (h *BCPHandler) Players(c echo.Context) error {
	players, err := h.client.FetchPlayers(c.Request().Context(), c.Param("id"))
	if err != nil {
		return bcpError(c, err)
	}
	cacheFor(c, false)
	return c.JSON(http.StatusOK, players)
}

// maxConcurrentRounds bounds how many of a multi-round pairings request
// are fetched from BCP at once.
//
// Same reasoning and same low number as me.go's maxConcurrentEventInfo:
// the request count is identical either way — these are the same rounds
// that used to be fetched one per HTTP request — but BCP shouldn't see
// one page load as a burst. What this replaces is worse than a burst
// anyway: the frontend was walking rounds in a serial `await` loop, so
// a five-round event meant five sequential browser round trips before
// anything rendered.
const maxConcurrentRounds = 3

// parseRounds reads either ?round=N or ?rounds=1,2,3 into a de-duplicated,
// ordered list. Both spellings are supported: the round board asks for
// exactly one, while "my pairings" and the placings round strip want a
// whole event's worth in a single call.
func parseRounds(c echo.Context) ([]int, error) {
	raw := c.QueryParam("rounds")
	if raw == "" {
		raw = c.QueryParam("round")
	}
	if raw == "" {
		return nil, fmt.Errorf(`"round" or "rounds" query param is required`)
	}

	seen := make(map[int]bool)
	rounds := make([]int, 0, 8)
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			return nil, fmt.Errorf(`each round must be a positive integer, got %q`, part)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		rounds = append(rounds, n)
	}
	if len(rounds) > maxRoundsPerRequest {
		return nil, fmt.Errorf("at most %d rounds per request, got %d", maxRoundsPerRequest, len(rounds))
	}
	return rounds, nil
}

// maxRoundsPerRequest caps one request's fan-out. Real events run to
// about eight rounds; this is a guard against a crafted URL turning one
// request into an unbounded crawl of BCP, not a limit anyone should
// reach.
const maxRoundsPerRequest = 20

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
	rounds, err := parseRounds(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	eventID := c.Param("id")
	if c.QueryParam("refresh") == "true" {
		for _, round := range rounds {
			h.client.InvalidateRoundPairings(eventID, pairingType, round)
		}
	}

	records, err := h.fetchRounds(c.Request().Context(), eventID, pairingType, rounds)
	if err != nil {
		return bcpError(c, err)
	}
	cacheFor(c, false)
	return c.JSON(http.StatusOK, records)
}

// fetchRounds resolves every requested round, a few at a time, and
// returns them as one flat list.
//
// Each record is stamped with the round it was fetched for when BCP
// didn't set one itself: PairingRecord.Round is a nullable field on
// their side, and a caller that asked for several rounds at once has no
// other way to tell them apart. Stamping what we asked for is not an
// inference — it is the only thing here we know for certain.
//
// One round failing fails the whole request, unlike the tolerate-and-
// skip paths elsewhere in this service. A partial pairings board is
// worse than an error: it looks like rounds that haven't happened yet.
func (h *BCPHandler) fetchRounds(ctx context.Context, eventID, pairingType string, rounds []int) ([]bcp.PairingRecord, error) {
	perRound := make([][]bcp.PairingRecord, len(rounds))
	errs := make([]error, len(rounds))

	sem := make(chan struct{}, maxConcurrentRounds)
	var wg sync.WaitGroup
	for i, round := range rounds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			records, err := h.client.FetchRoundPairings(ctx, eventID, pairingType, round)
			if err != nil {
				errs[i] = err
				return
			}
			for j := range records {
				if records[j].Round == nil {
					r := round
					records[j].Round = &r
				}
			}
			perRound[i] = records
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// Flattened in the order asked for, not completion order, so the
	// response is stable.
	out := make([]bcp.PairingRecord, 0, len(rounds)*8)
	for _, records := range perRound {
		out = append(out, records...)
	}
	return out, nil
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
	cacheFor(c, false)
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
	// A season aggregate, held for itcRankingRefetchInterval server-side
	// (see internal/bcp/client.go). The browser gets the short interval
	// rather than that full window: these are cheap responses, and the
	// expensive part — the BCP request — is already avoided by the
	// durable cache regardless of what the browser does.
	cacheFor(c, false)
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
