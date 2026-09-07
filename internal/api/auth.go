package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/auth"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

const (
	sessionCookieName = "session"
	stateCookieName   = "oauth_state"
	stateCookieMaxAge = 10 * time.Minute
)

// userStore is the persistence RegisterAuthRoutes needs. *user.Store
// satisfies it in production; tests satisfy it with an in-memory fake,
// so these routes' cookie/redirect/error-mapping logic can be tested
// without a real database — the same interface-at-the-point-of-use
// pattern as internal/api/bcp.go's *bcp.Client dependency.
type userStore interface {
	UpsertUserFromGoogle(ctx context.Context, googleSub, email, name, avatarURL string) (user.User, error)
	CreateSession(ctx context.Context, userID int64) (string, error)
	GetUserBySession(ctx context.Context, token string) (user.User, error)
	DeleteSession(ctx context.Context, token string) error
	// SetBcpUserID is used by RegisterMeRoutes (internal/api/me.go), not
	// by anything in this file — declared here anyway since userStore is
	// this package's one shared "what RegisterAuthRoutes' store argument
	// needs" interface, and every route that touches the signed-in
	// user's row goes through it.
	SetBcpUserID(ctx context.Context, userID int64, bcpUserID string) error
	// The following are used by RegisterSyncRoutes (internal/api/sync.go)
	// for the same reason SetBcpUserID is declared here rather than in
	// its own file's own interface.
	ListFollows(ctx context.Context, userID int64, eventID string) ([]user.Follow, error)
	AddFollow(ctx context.Context, userID int64, eventID, kind, refID, label string) error
	RemoveFollow(ctx context.Context, userID int64, eventID, kind, refID string) error
	ListRecentEvents(ctx context.Context, userID int64) ([]user.RecentEvent, error)
	RecordRecentEvent(ctx context.Context, userID int64, eventID, eventName string, teamEvent bool) error
}

// RegisterAuthRoutes wires up Google sign-in, sign-out, and the
// signed-in-user lookup the frontend calls on load.
//
// cookieSecure should be true for any real (HTTPS) deployment and false
// for local http://localhost development — browsers refuse to store a
// Secure cookie at all over plain HTTP, which would otherwise silently
// break sign-in locally.
func RegisterAuthRoutes(e *echo.Echo, google *auth.GoogleOAuth, store userStore, frontendURL string, cookieSecure bool) {
	e.GET("/auth/google/login", func(c echo.Context) error {
		state, err := auth.NewState()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		setCookie(c, stateCookieName, state, stateCookieMaxAge, cookieSecure)
		return c.Redirect(http.StatusFound, google.AuthCodeURL(state))
	})

	e.GET("/auth/google/callback", func(c echo.Context) error {
		// The state cookie is single-use either way — cleared whether
		// this turns out to be a match or not.
		stateCookie, cookieErr := c.Cookie(stateCookieName)
		clearCookie(c, stateCookieName, cookieSecure)

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
		accessToken, err := google.Exchange(c.Request().Context(), code)
		if err != nil {
			log.Printf("google callback: token exchange failed: %v", err)
			return c.String(http.StatusBadGateway, "Google sign-in failed: "+err.Error())
		}
		info, err := google.FetchUserInfo(c.Request().Context(), accessToken)
		if err != nil {
			log.Printf("google callback: fetching user info failed: %v", err)
			return c.String(http.StatusBadGateway, "Google sign-in failed: "+err.Error())
		}

		u, err := store.UpsertUserFromGoogle(c.Request().Context(), info.Sub, info.Email, info.Name, info.Picture)
		if err != nil {
			log.Printf("google callback: upserting user %s failed: %v", info.Email, err)
			return c.String(http.StatusInternalServerError, "sign-in failed: "+err.Error())
		}
		sessionToken, err := store.CreateSession(c.Request().Context(), u.ID)
		if err != nil {
			log.Printf("google callback: creating session for user %d failed: %v", u.ID, err)
			return c.String(http.StatusInternalServerError, "sign-in failed: "+err.Error())
		}

		log.Printf("google callback: signed in user %d (%s)", u.ID, u.Email)
		setCookie(c, sessionCookieName, sessionToken, user.SessionDuration, cookieSecure)
		return c.Redirect(http.StatusFound, frontendURL)
	})

	e.POST("/auth/logout", func(c echo.Context) error {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			// Best-effort: whether or not the row still existed, the
			// caller's desired end state (no valid session) now holds.
			if err := store.DeleteSession(c.Request().Context(), cookie.Value); err != nil {
				log.Printf("logout: deleting session failed (proceeding anyway): %v", err)
			}
		}
		clearCookie(c, sessionCookieName, cookieSecure)
		return c.NoContent(http.StatusNoContent)
	})

	e.GET("/api/me", func(c echo.Context) error {
		cookie, err := c.Cookie(sessionCookieName)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		}
		u, err := store.GetUserBySession(c.Request().Context(), cookie.Value)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"id":        u.ID,
			"email":     u.Email,
			"name":      u.Name,
			"avatarUrl": u.AvatarURL,
			"bcpUserId": u.BcpUserID,
		})
	})
}

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
