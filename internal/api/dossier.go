package api

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// dossierResponse is GET /api/players/:bcpUserId/dossier's body: the
// same faction/placing summary GET /api/players/:bcpUserId/stats already
// computes (see statsForBcpUser in stats.go), plus the name of the Brass
// Ledger account that linked this BCP user id — BCP's own API has no
// standalone "player profile" lookup by id (a name only ever comes back
// attached to an event roster, see internal/bcp/players.go), so this
// uses the same display name the account itself signed in with, the
// only name this service has for someone independent of any one event.
type dossierResponse struct {
	Name string `json:"name"`
	playerStatsResponse
}

// DossierHandler serves a public, opt-out player dossier page: whatever
// GET /api/players/:bcpUserId/stats already computes for a signed-in
// caller, made reachable by anyone (signed in or not) for an account
// that hasn't turned it off — see migration 0014 and
// User.DossierPublic's doc comment. A thin wrapper around statsForBcpUser
// (stats.go) plus one store lookup; no BCP data here that a signed-in
// user couldn't already see per-player via the existing stats route.
type DossierHandler struct {
	store  userStore
	client bcpClient
}

// NewDossierHandler builds a DossierHandler.
func NewDossierHandler(store userStore, client bcpClient) *DossierHandler {
	return &DossierHandler{store: store, client: client}
}

// Register wires this handler's route onto e. Deliberately public — no
// RequireSession/RequireApproved — since the whole point is a link
// shareable with someone who's never signed in. rateLimit is applied
// just to this route, same reason and shape as FeedbackHandler's own
// (see main.go): an anonymous caller can otherwise hit real BCP-backed
// work (statsForBcpUser) with no session standing between them and it.
func (h *DossierHandler) Register(e *echo.Echo, rateLimit echo.MiddlewareFunc) {
	e.GET("/api/players/:bcpUserId/dossier", h.Dossier, rateLimit)
}

// Dossier is GET /api/players/:bcpUserId/dossier. Returns 404 for a
// bcpUserId with no linked account, an unapproved account, or one that's
// turned dossierPublic off — deliberately the same 404 for all three
// rather than distinguishing "doesn't exist" from "exists but private",
// so a visitor can't use this route to enumerate who has or hasn't opted
// out.
func (h *DossierHandler) Dossier(c echo.Context) error {
	bcpUserID := c.Param("bcpUserId")
	if bcpUserID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "bcpUserId is required"})
	}

	ctx := c.Request().Context()
	acct, err := h.store.GetUserByBcpUserID(ctx, bcpUserID)
	if err != nil {
		if errors.Is(err, user.ErrUserNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "dossier not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if acct.Status != user.StatusApproved || !acct.DossierPublic {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "dossier not found"})
	}

	// Always the full computation: the dossier page is what renders the
	// Team/GT/RTT tiles and the "of N" field sizes in the first place.
	stats, err := statsForBcpUser(ctx, h.client, bcpUserID, withEventDetail)
	if err != nil {
		return bcpError(c, err)
	}

	return c.JSON(http.StatusOK, dossierResponse{Name: acct.Name, playerStatsResponse: stats})
}
