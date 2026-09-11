package api

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/auth"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

const (
	sessionCookieName  = "session"
	stateCookieName    = "oauth_state"
	returnToCookieName = "oauth_return_to"
	stateCookieMaxAge  = 10 * time.Minute
)

// isSafeReturnPath reports whether path is safe to redirect the browser
// to after sign-in — a same-origin relative path, nothing else. This is
// the one thing standing between "remember where the user was" and an
// open-redirect vector: rejects an empty path, anything not starting
// with "/", a protocol-relative path ("//evil.com" — browsers treat that
// as a same-scheme link to a different host), and anything containing
// "://" (a full absolute URL to somewhere else entirely).
func isSafeReturnPath(path string) bool {
	if path == "" || path[0] != '/' || strings.HasPrefix(path, "//") {
		return false
	}
	return !strings.Contains(path, "://")
}

// userStore is the persistence this package's handlers need. *user.Store
// satisfies it in production; tests satisfy it with an in-memory fake,
// so cookie/redirect/error-mapping logic can be tested without a real
// database — the same interface-at-the-point-of-use pattern as deps.go's
// bcpClient.
type userStore interface {
	UpsertUserFromGoogle(ctx context.Context, googleSub, email, name, avatarURL string) (user.User, error)
	CreateSession(ctx context.Context, userID int64) (string, error)
	GetUserBySession(ctx context.Context, token string) (user.User, error)
	DeleteSession(ctx context.Context, token string) error
	// SetBcpUserID is used by MeHandler (internal/api/me.go), not by
	// anything in this file — declared here anyway since userStore is
	// this package's one shared "what a handler's store field needs"
	// interface, and every handler that touches the signed-in user's row
	// goes through it.
	SetBcpUserID(ctx context.Context, userID int64, bcpUserID string) error
	// The following are used by SyncHandler (internal/api/sync.go) for
	// the same reason SetBcpUserID is declared here rather than in its
	// own file's own interface.
	ListFollows(ctx context.Context, userID int64, eventID string) ([]user.Follow, error)
	AddFollow(ctx context.Context, userID int64, eventID, kind, refID, label string) error
	RemoveFollow(ctx context.Context, userID int64, eventID, kind, refID string) error
	ListRecentEvents(ctx context.Context, userID int64) ([]user.RecentEvent, error)
	RecordRecentEvent(ctx context.Context, userID int64, eventID, eventName string, teamEvent bool) error
	// The following are used by AdminHandler (internal/api/admin.go) and
	// this file's own ADMIN_EMAILS bootstrap in Callback below — same
	// reason as the rest of this interface's later additions.
	SetStatus(ctx context.Context, userID int64, status string) error
	SetRole(ctx context.Context, userID int64, role string) error
	ListUsers(ctx context.Context) ([]user.User, error)
	// SetThemePreference is used by MeHandler (internal/api/me.go), same
	// reason as SetBcpUserID above.
	SetThemePreference(ctx context.Context, userID int64, theme string) error
}

// AuthHandler wires up Google sign-in, sign-out, and the signed-in-user
// lookup the frontend calls on load.
type AuthHandler struct {
	google       *auth.GoogleOAuth
	store        userStore
	frontendURL  string
	cookieSecure bool
	// adminEmails bootstraps access control — see Config.AdminEmails'
	// doc comment and this file's Callback, the only place it's read.
	// Already lowercased by config.parseAdminEmails; Callback lowercases
	// the signed-in email the same way before comparing.
	adminEmails []string
}

// NewAuthHandler builds an AuthHandler.
//
// cookieSecure should be true for any real (HTTPS) deployment and false
// for local http://localhost development — browsers refuse to store a
// Secure cookie at all over plain HTTP, which would otherwise silently
// break sign-in locally.
func NewAuthHandler(google *auth.GoogleOAuth, store userStore, frontendURL string, cookieSecure bool, adminEmails []string) *AuthHandler {
	return &AuthHandler{
		google: google,
		store:  store,
		// Normalized once here rather than trusting callers/config not to
		// include one — a trailing slash would otherwise turn
		// frontendURL+"/welcome" into a double slash.
		frontendURL:  strings.TrimSuffix(frontendURL, "/"),
		cookieSecure: cookieSecure,
		adminEmails:  adminEmails,
	}
}

// Register wires this handler's routes onto e.
func (h *AuthHandler) Register(e *echo.Echo) {
	e.GET("/auth/google/login", h.Login)
	e.GET("/auth/google/callback", h.Callback)
	e.POST("/auth/logout", h.Logout)
	e.GET("/api/me", h.Me)
}

// Login is GET /auth/google/login: redirects to Google's consent screen.
func (h *AuthHandler) Login(c echo.Context) error {
	state, err := auth.NewState()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	setCookie(c, stateCookieName, state, stateCookieMaxAge, h.cookieSecure)

	// Optional: the frontend passes ?return_to=<path> so a returning,
	// already-linked user signing back in from (say) /stats lands
	// back on /stats instead of always the homepage. Silently ignored
	// if unsafe or absent — see isSafeReturnPath. Never honored for a
	// not-yet-linked account either way (see Callback below),
	// since that always goes through onboarding first.
	if returnTo := c.QueryParam("return_to"); isSafeReturnPath(returnTo) {
		setCookie(c, returnToCookieName, returnTo, stateCookieMaxAge, h.cookieSecure)
	}

	return c.Redirect(http.StatusFound, h.google.AuthCodeURL(state))
}

// Callback is GET /auth/google/callback: completes the OAuth flow and
// starts a session.
func (h *AuthHandler) Callback(c echo.Context) error {
	// Both cookies are single-use — cleared regardless of how this
	// request turns out.
	stateCookie, cookieErr := c.Cookie(stateCookieName)
	clearCookie(c, stateCookieName, h.cookieSecure)
	returnToCookie, returnToErr := c.Cookie(returnToCookieName)
	clearCookie(c, returnToCookieName, h.cookieSecure)

	if cookieErr != nil || c.QueryParam("state") == "" || stateCookie.Value != c.QueryParam("state") {
		// Logged at the level a normal, expected-to-happen-sometimes
		// event deserves (an expired 10-minute state cookie, a
		// double-click, someone poking the callback URL directly) —
		// not an error. Never logs the state values themselves;
		// "match/no match" is all that matters here.
		log.Printf("google callback: state check failed (cookie present: %v, query state present: %v)",
			cookieErr == nil, c.QueryParam("state") != "")
		return c.String(http.StatusBadRequest, "invalid or expired sign-in attempt — please try signing in again")
	}

	code := c.QueryParam("code")
	if code == "" {
		log.Printf("google callback: missing authorization code")
		return c.String(http.StatusBadRequest, "missing authorization code")
	}

	// Never logs the authorization code or the access token it
	// exchanges for — only that the exchange happened and whether it
	// succeeded — since either one is a live, usable credential for
	// as long as it hasn't expired.
	accessToken, err := h.google.Exchange(c.Request().Context(), code)
	if err != nil {
		log.Printf("google callback: token exchange failed: %v", err)
		return c.String(http.StatusBadGateway, "Google sign-in failed: "+err.Error())
	}
	info, err := h.google.FetchUserInfo(c.Request().Context(), accessToken)
	if err != nil {
		log.Printf("google callback: fetching user info failed: %v", err)
		return c.String(http.StatusBadGateway, "Google sign-in failed: "+err.Error())
	}

	u, err := h.store.UpsertUserFromGoogle(c.Request().Context(), info.Sub, info.Email, info.Name, info.Picture)
	if err != nil {
		log.Printf("google callback: upserting user %s failed: %v", info.Email, err)
		return c.String(http.StatusInternalServerError, "sign-in failed: "+err.Error())
	}

	// ADMIN_EMAILS bootstrap — applied on every sign-in, not just the
	// first, so it also covers promoting an account that already existed
	// before being added to the list. See Config.AdminEmails' doc
	// comment for why this always wins rather than only applying once.
	if isAdminEmail(u.Email, h.adminEmails) && (u.Role != user.RoleAdmin || u.Status != user.StatusApproved) {
		if err := h.store.SetRole(c.Request().Context(), u.ID, user.RoleAdmin); err != nil {
			log.Printf("google callback: promoting admin %s failed: %v", u.Email, err)
		} else if err := h.store.SetStatus(c.Request().Context(), u.ID, user.StatusApproved); err != nil {
			log.Printf("google callback: approving admin %s failed: %v", u.Email, err)
		} else {
			log.Printf("google callback: %s matched ADMIN_EMAILS, promoted to admin+approved", u.Email)
			u.Role, u.Status = user.RoleAdmin, user.StatusApproved
		}
	}

	sessionToken, err := h.store.CreateSession(c.Request().Context(), u.ID)
	if err != nil {
		log.Printf("google callback: creating session for user %d failed: %v", u.ID, err)
		return c.String(http.StatusInternalServerError, "sign-in failed: "+err.Error())
	}

	log.Printf("google callback: signed in user %d (%s)", u.ID, u.Email)
	setCookie(c, sessionCookieName, sessionToken, user.SessionDuration, h.cookieSecure)

	redirectTo := h.frontendURL
	switch {
	case u.BcpUserID == "":
		// First sign-in ever, or an existing account that's never
		// gotten around to linking a BCP profile — either way, send
		// them through the dedicated onboarding page instead of the
		// plain homepage, so connecting a profile (and then seeing
		// their event history) is the very next thing that happens
		// rather than something they have to go find themselves.
		// return_to is deliberately ignored on this path: onboarding
		// always comes first regardless of where sign-in started.
		redirectTo = h.frontendURL + "/welcome"
	case returnToErr == nil && isSafeReturnPath(returnToCookie.Value):
		redirectTo = h.frontendURL + returnToCookie.Value
	}
	return c.Redirect(http.StatusFound, redirectTo)
}

// isAdminEmail reports whether email matches one of adminEmails,
// case-insensitively — adminEmails is already lowercased by
// config.parseAdminEmails, so only email needs normalizing here.
func isAdminEmail(email string, adminEmails []string) bool {
	return slices.Contains(adminEmails, strings.ToLower(email))
}

// Logout is POST /auth/logout: ends the caller's session.
func (h *AuthHandler) Logout(c echo.Context) error {
	if cookie, err := c.Cookie(sessionCookieName); err == nil {
		// Best-effort: whether or not the row still existed, the
		// caller's desired end state (no valid session) now holds.
		if err := h.store.DeleteSession(c.Request().Context(), cookie.Value); err != nil {
			log.Printf("logout: deleting session failed (proceeding anyway): %v", err)
		}
	}
	clearCookie(c, sessionCookieName, h.cookieSecure)
	return c.NoContent(http.StatusNoContent)
}

// Me is GET /api/me: returns the signed-in user's own profile —
// deliberately just a valid session, no approval requirement (unlike
// RequireApproved below), since this is exactly how the frontend learns
// whether a signed-in visitor is pending/rejected in the first place.
func (h *AuthHandler) Me(c echo.Context) error {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}
	u, err := h.store.GetUserBySession(c.Request().Context(), cookie.Value)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"id":              u.ID,
		"email":           u.Email,
		"name":            u.Name,
		"avatarUrl":       u.AvatarURL,
		"bcpUserId":       u.BcpUserID,
		"role":            u.Role,
		"status":          u.Status,
		"themePreference": u.ThemePreference,
	})
}

// RequireSession is Echo middleware gating a route behind a valid
// session cookie alone — authentication, not authorization (see
// RequireApproved below for the stricter, "and approved" version most
// routes actually want). Applied at the routing layer so it can cover
// routes whose handlers were never written to know about sessions at
// all. The one current use: BCPHandler's Players route, which
// deliberately stays at this weaker bar — see its Register call in
// cmd/server/main.go for why.
//
// On success, the authenticated user is stashed in the request context
// under contextKeyUser for any handler (or wrapping middleware, see
// RequireApproved) that wants it.
func RequireSession(store userStore) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cookie, err := c.Cookie(sessionCookieName)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			}
			u, err := store.GetUserBySession(c.Request().Context(), cookie.Value)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			}
			c.Set(contextKeyUser, u)
			return next(c)
		}
	}
}

// RequireApproved is RequireSession plus Status == StatusApproved — the
// authorization layer on top of authentication (see migration 0007's
// comment for why the two are deliberately not the same check), and
// what every BCPHandler route except Players is wrapped in (see
// cmd/server/main.go). requireApprovedUser (sync.go) is the equivalent
// for a handler that needs the user value directly rather than through
// middleware.
func RequireApproved(store userStore) echo.MiddlewareFunc {
	requireSession := RequireSession(store)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return requireSession(func(c echo.Context) error {
			u, _ := c.Get(contextKeyUser).(user.User)
			if u.Status != user.StatusApproved {
				return c.JSON(http.StatusForbidden, map[string]string{
					"error":  "account not approved",
					"status": u.Status,
				})
			}
			return next(c)
		})
	}
}

const contextKeyUser = "user"

func setCookie(c echo.Context, name, value string, maxAge time.Duration, secure bool) {
	c.SetCookie(&http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(c echo.Context, name string, secure bool) {
	c.SetCookie(&http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}
