package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/follow"
)

// followTokenHeader carries a follow link's token on event-data requests
// from someone viewing the event through that link.
const followTokenHeader = "X-Follow-Token"

// itcRankingsPath is the one event-data route without an event id in its
// path; follow-token requests to it name the event in ?eventId=.
const itcRankingsPath = "/api/itc/rankings"

const (
	// How long after an event ends its links and spectated rows last,
	// so final placings can still be looked at.
	followGracePeriod = 7 * 24 * time.Hour
	// Expiry for an event BCP gives no dates for.
	followUndatedLifetime = 60 * 24 * time.Hour
)

type followStore interface {
	EnsureLink(ctx context.Context, userID int64, eventID, playerID string, expiresAt time.Time) (follow.Link, error)
	GetLinkForUser(ctx context.Context, userID int64, eventID string) (follow.Link, error)
	GetLinkByToken(ctx context.Context, token string) (follow.Link, error)
	DeleteLink(ctx context.Context, userID int64, eventID string) error
	SaveSpectating(ctx context.Context, userID int64, sp follow.Spectated) error
	ListSpectating(ctx context.Context, userID int64) ([]follow.Spectated, error)
	GetSpectating(ctx context.Context, userID int64, eventID string) (follow.Spectated, error)
	DeleteSpectating(ctx context.Context, userID int64, eventID string) error
}

// followExpiry is when a link or spectated row for this event stops
// working.
func followExpiry(info bcp.EventInfo, now time.Time) time.Time {
	for _, s := range []string{info.EndDate, info.StartDate} {
		if t, ok := bcp.ParseDate(s); ok {
			return t.Add(followGracePeriod)
		}
	}
	return now.Add(followUndatedLifetime)
}

func rosterEntryForBcpUser(players []bcp.Player, bcpUserID string) (bcp.Player, bool) {
	if bcpUserID == "" {
		return bcp.Player{}, false
	}
	i := slices.IndexFunc(players, func(p bcp.Player) bool { return p.BcpUserID == bcpUserID })
	if i < 0 {
		return bcp.Player{}, false
	}
	return players[i], true
}

func rosterEntryByID(players []bcp.Player, playerID string) (bcp.Player, bool) {
	i := slices.IndexFunc(players, func(p bcp.Player) bool { return p.ID == playerID })
	if i < 0 {
		return bcp.Player{}, false
	}
	return players[i], true
}

// --- Token lookups -------------------------------------------------------

// linkCacheTTL bounds how long a revoked link keeps working for requests
// already in flight from another page. Revoking through this service
// clears the cache at once; the TTL only matters for expiry.
const (
	linkCacheTTL        = 30 * time.Second
	linkCacheMaxEntries = 1024
)

type cachedLink struct {
	link  follow.Link
	found bool
	until time.Time
}

// FollowAccess lets a valid follow token stand in for a session on the
// event-data routes, for that token's own event only.
type FollowAccess struct {
	links   followStore
	client  bcpClient
	limiter echo.MiddlewareFunc

	mu    sync.Mutex
	cache map[string]cachedLink
}

// NewFollowAccess builds a FollowAccess. limiter applies to requests let
// through by a token, which have no account behind them.
func NewFollowAccess(links followStore, client bcpClient, limiter echo.MiddlewareFunc) *FollowAccess {
	return &FollowAccess{links: links, client: client, limiter: limiter, cache: map[string]cachedLink{}}
}

func (f *FollowAccess) lookup(ctx context.Context, token string) (follow.Link, bool, error) {
	f.mu.Lock()
	if e, ok := f.cache[token]; ok && time.Now().Before(e.until) {
		f.mu.Unlock()
		return e.link, e.found, nil
	}
	f.mu.Unlock()

	link, err := f.links.GetLinkByToken(ctx, token)
	found := err == nil
	if err != nil && !errors.Is(err, follow.ErrNotFound) {
		return follow.Link{}, false, err
	}
	if found && time.Now().After(link.ExpiresAt) {
		found = false
	}

	f.mu.Lock()
	if len(f.cache) >= linkCacheMaxEntries {
		clear(f.cache)
	}
	f.cache[token] = cachedLink{link: link, found: found, until: time.Now().Add(linkCacheTTL)}
	f.mu.Unlock()
	return link, found, nil
}

func (f *FollowAccess) forget() {
	f.mu.Lock()
	clear(f.cache)
	f.mu.Unlock()
}

func followEventID(c echo.Context) string {
	if id := c.Param("id"); id != "" {
		return id
	}
	if id := c.Param("eventId"); id != "" {
		return id
	}
	return c.QueryParam("eventId")
}

// Or returns base, except that a request carrying a valid follow token
// for the event it asks about is let through without a session.
func (f *FollowAccess) Or(base echo.MiddlewareFunc) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		viaSession := base(next)
		viaToken := f.limiter(next)
		return func(c echo.Context) error {
			token := strings.TrimSpace(c.Request().Header.Get(followTokenHeader))
			if token == "" {
				return viaSession(c)
			}
			ok, err := f.allows(c, token)
			if err != nil {
				return internalError(c, err)
			}
			if !ok {
				return viaSession(c)
			}
			return viaToken(c)
		}
	}
}

func (f *FollowAccess) allows(c echo.Context, token string) (bool, error) {
	ctx := c.Request().Context()
	link, found, err := f.lookup(ctx, token)
	if err != nil || !found {
		return false, err
	}
	eventID := followEventID(c)
	if eventID == "" || eventID != link.EventID {
		return false, nil
	}
	if c.Path() != itcRankingsPath {
		return true, nil
	}

	// A ranking is per player, so the token only covers this event's
	// own league and roster.
	leagueIDs, err := f.client.FetchEventLeagueIDs(ctx, eventID)
	if err != nil {
		return false, nil
	}
	if !slices.Contains(leagueIDs, c.QueryParam("leagueId")) {
		return false, nil
	}
	players, err := f.client.FetchPlayers(ctx, eventID)
	if err != nil {
		return false, nil
	}
	_, onRoster := rosterEntryForBcpUser(players, c.QueryParam("userId"))
	return onRoster, nil
}

// --- Routes ----------------------------------------------------------------

// FollowHandler serves follow links and the signed-in user's spectated
// events.
type FollowHandler struct {
	store  userStore
	links  followStore
	client bcpClient
	access *FollowAccess
}

// NewFollowHandler builds a FollowHandler. access is told when a link is
// revoked, so the revocation takes effect at once.
func NewFollowHandler(store userStore, links followStore, client bcpClient, access *FollowAccess) *FollowHandler {
	return &FollowHandler{store: store, links: links, client: client, access: access}
}

// Register wires this handler's routes onto e. The token lookup is
// public, so it takes rateLimit.
func (h *FollowHandler) Register(e *echo.Echo, rateLimit echo.MiddlewareFunc) {
	e.GET("/api/events/:id/follow-link", h.GetLink)
	e.PUT("/api/events/:id/follow-link", h.EnsureLink)
	e.DELETE("/api/events/:id/follow-link", h.DeleteLink)
	e.GET("/api/follow/:token", h.ResolveToken, rateLimit)
	e.GET("/api/me/spectating", h.ListSpectating)
	e.POST("/api/me/spectating", h.SaveSpectating)
	e.GET("/api/me/spectating/:eventId", h.GetSpectating)
	e.DELETE("/api/me/spectating/:eventId", h.DeleteSpectating)
}

type followLinkResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

func linkResponse(l follow.Link) followLinkResponse {
	return followLinkResponse{Token: l.Token, ExpiresAt: l.ExpiresAt.UTC().Format(time.RFC3339)}
}

var errNotFoundJSON = map[string]string{"error": "not found"}

// errEventOverJSON answers a request to follow an event whose links would
// already have expired.
var errEventOverJSON = map[string]string{"error": "This event finished more than a week ago."}

// GetLink is GET /api/events/:id/follow-link: the caller's live link for
// this event, or null. Having no link is a normal answer, so it isn't a 404
// that browsers would log as an error on every Mine tab.
func (h *FollowHandler) GetLink(c echo.Context) error {
	noCache(c)
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	link, err := h.links.GetLinkForUser(c.Request().Context(), u.ID, c.Param("id"))
	if errors.Is(err, follow.ErrNotFound) {
		return c.JSON(http.StatusOK, nil)
	}
	if err != nil {
		return internalError(c, err)
	}
	return c.JSON(http.StatusOK, linkResponse(link))
}

// EnsureLink is PUT /api/events/:id/follow-link: creates the caller's
// link for this event, or returns the one they already have. Only a
// player on the event's roster can share their own view of it.
func (h *FollowHandler) EnsureLink(c echo.Context) error {
	noCache(c)
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	if u.BcpUserID == "" {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "Link your Best Coast Pairings profile to share a follow link."})
	}
	ctx := c.Request().Context()
	eventID := c.Param("id")

	players, err := h.client.FetchPlayers(ctx, eventID)
	if err != nil {
		return bcpError(c, err)
	}
	me, ok := rosterEntryForBcpUser(players, u.BcpUserID)
	if !ok {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "You can only share a follow link for an event you're registered for."})
	}
	info, err := h.client.FetchEventInfo(ctx, eventID)
	if err != nil {
		return bcpError(c, err)
	}

	expiresAt := followExpiry(info, time.Now())
	if !expiresAt.After(time.Now()) {
		return c.JSON(http.StatusConflict, errEventOverJSON)
	}
	link, err := h.links.EnsureLink(ctx, u.ID, eventID, me.ID, expiresAt)
	if err != nil {
		return internalError(c, err)
	}
	return c.JSON(http.StatusOK, linkResponse(link))
}

// DeleteLink is DELETE /api/events/:id/follow-link: turns the caller's
// link off. Spectators who saved it lose the event from their list.
func (h *FollowHandler) DeleteLink(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	if err := h.links.DeleteLink(c.Request().Context(), u.ID, c.Param("id")); err != nil {
		return internalError(c, err)
	}
	h.access.forget()
	return c.NoContent(http.StatusNoContent)
}

type resolvedFollowLink struct {
	EventID  string `json:"eventId"`
	PlayerID string `json:"playerId"`
}

// ResolveToken is GET /api/follow/:token, which needs no sign-in. A
// missing, revoked or expired token all get the same 404.
func (h *FollowHandler) ResolveToken(c echo.Context) error {
	noCache(c)
	link, found, err := h.access.lookup(c.Request().Context(), c.Param("token"))
	if err != nil {
		return internalError(c, err)
	}
	if !found {
		return c.JSON(http.StatusNotFound, errNotFoundJSON)
	}
	return c.JSON(http.StatusOK, resolvedFollowLink{EventID: link.EventID, PlayerID: link.PlayerID})
}

type saveSpectatingRequest struct {
	Token    string `json:"token"`
	EventID  string `json:"eventId"`
	PlayerID string `json:"playerId"`
}

type saveSpectatingResponse struct {
	Saved bool `json:"saved"`
}

// SaveSpectating is POST /api/me/spectating, from a follow link
// ({token}) or from picking a player ({eventId, playerId}). Nothing is
// saved when the token is the caller's own or the caller is on the
// event's roster, since then the event is already theirs.
func (h *FollowHandler) SaveSpectating(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	var req saveSpectatingRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	ctx := c.Request().Context()

	sp := follow.Spectated{EventID: strings.TrimSpace(req.EventID), PlayerID: strings.TrimSpace(req.PlayerID)}
	if token := strings.TrimSpace(req.Token); token != "" {
		link, found, err := h.access.lookup(ctx, token)
		if err != nil {
			return internalError(c, err)
		}
		if !found {
			return c.JSON(http.StatusNotFound, errNotFoundJSON)
		}
		if link.UserID == u.ID {
			return c.JSON(http.StatusOK, saveSpectatingResponse{Saved: false})
		}
		linkID := link.ID
		sp = follow.Spectated{EventID: link.EventID, PlayerID: link.PlayerID, FollowLinkID: &linkID, ExpiresAt: link.ExpiresAt}
	} else if sp.EventID == "" || sp.PlayerID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "token, or eventId and playerId, are required"})
	}

	players, err := h.client.FetchPlayers(ctx, sp.EventID)
	if err != nil {
		return bcpError(c, err)
	}
	if _, playing := rosterEntryForBcpUser(players, u.BcpUserID); playing {
		return c.JSON(http.StatusOK, saveSpectatingResponse{Saved: false})
	}
	if _, ok := rosterEntryByID(players, sp.PlayerID); !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "That player isn't on this event's roster."})
	}
	if sp.FollowLinkID == nil {
		info, err := h.client.FetchEventInfo(ctx, sp.EventID)
		if err != nil {
			return bcpError(c, err)
		}
		sp.ExpiresAt = followExpiry(info, time.Now())
		if !sp.ExpiresAt.After(time.Now()) {
			return c.JSON(http.StatusConflict, errEventOverJSON)
		}
	}

	if err := h.links.SaveSpectating(ctx, u.ID, sp); err != nil {
		return internalError(c, err)
	}
	return c.JSON(http.StatusOK, saveSpectatingResponse{Saved: true})
}

type spectatedEventResponse struct {
	EventID    string `json:"eventId"`
	EventName  string `json:"eventName"`
	StartDate  string `json:"startDate,omitempty"`
	EndDate    string `json:"endDate,omitempty"`
	TeamEvent  bool   `json:"teamEvent"`
	Started    bool   `json:"started"`
	Ended      bool   `json:"ended"`
	PlayerID   string `json:"playerId"`
	PlayerName string `json:"playerName"`
	ViaLink    bool   `json:"viaLink"`
}

type spectatingListResponse struct {
	Now      []spectatedEventResponse `json:"now"`
	Upcoming []spectatedEventResponse `json:"upcoming"`
}

// ListSpectating is GET /api/me/spectating. Events the caller has since
// registered for are left out: they show under the caller's own events.
// Event and player names come from BCP on each read and aren't stored.
func (h *FollowHandler) ListSpectating(c echo.Context) error {
	noCache(c)
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	rows, err := h.links.ListSpectating(ctx, u.ID)
	if err != nil {
		return internalError(c, err)
	}

	eventIDs := make([]string, len(rows))
	for i, r := range rows {
		eventIDs[i] = r.EventID
	}
	h.client.PrewarmEventInfo(ctx, eventIDs)

	type result struct {
		item spectatedEventResponse
		keep bool
	}
	results := make([]result, len(rows))
	var wg sync.WaitGroup
	for i, r := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item := spectatedEventResponse{EventID: r.EventID, PlayerID: r.PlayerID, ViaLink: r.FollowLinkID != nil}
			// A failed lookup still lists the event, so it can be opened
			// or removed; the names fill in on a later visit.
			if info, err := h.client.FetchEventInfo(ctx, r.EventID); err == nil {
				item.EventName, item.StartDate, item.EndDate = info.Name, info.StartDate, info.EndDate
				item.TeamEvent, item.Started, item.Ended = info.TeamEvent, info.Started, info.Ended
			} else {
				log.Printf("%s %s: BCP event info: %v", c.Request().Method, c.Path(), err)
			}
			if players, err := h.client.FetchPlayers(ctx, r.EventID); err == nil {
				if _, playing := rosterEntryForBcpUser(players, u.BcpUserID); playing {
					return
				}
				if p, ok := rosterEntryByID(players, r.PlayerID); ok {
					item.PlayerName = p.Name
				}
			} else {
				log.Printf("%s %s: BCP players: %v", c.Request().Method, c.Path(), err)
			}
			results[i] = result{item: item, keep: true}
		}()
	}
	wg.Wait()

	resp := spectatingListResponse{Now: []spectatedEventResponse{}, Upcoming: []spectatedEventResponse{}}
	for _, r := range results {
		if !r.keep {
			continue
		}
		if r.item.Started {
			resp.Now = append(resp.Now, r.item)
		} else {
			resp.Upcoming = append(resp.Upcoming, r.item)
		}
	}
	return c.JSON(http.StatusOK, resp)
}

type spectatingOneResponse struct {
	PlayerID string `json:"playerId"`
}

// GetSpectating is GET /api/me/spectating/:eventId: who the caller is
// following in this event, or null. Every event page asks, so "nobody" is a
// normal answer rather than a 404 logged as an error.
func (h *FollowHandler) GetSpectating(c echo.Context) error {
	noCache(c)
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	sp, err := h.links.GetSpectating(c.Request().Context(), u.ID, c.Param("eventId"))
	if errors.Is(err, follow.ErrNotFound) {
		return c.JSON(http.StatusOK, nil)
	}
	if err != nil {
		return internalError(c, err)
	}
	return c.JSON(http.StatusOK, spectatingOneResponse{PlayerID: sp.PlayerID})
}

// DeleteSpectating is DELETE /api/me/spectating/:eventId.
func (h *FollowHandler) DeleteSpectating(c echo.Context) error {
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}
	if err := h.links.DeleteSpectating(c.Request().Context(), u.ID, c.Param("eventId")); err != nil {
		return internalError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
