package api

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// RejectForeignOrigin refuses state-changing requests that a browser sent
// from any origin other than the frontend. The session cookie is
// SameSite=Lax, which still lets a page on any brass-ledger.app subdomain
// post to this API with it, and several routes take no body (approve,
// logout) or bind a form body as empty fields (unlinking a profile), so
// they need no CORS preflight. Browsers always send Origin on POST, PUT,
// PATCH and DELETE; a request without one is not from a browser page and
// carries no ambient cookie risk, so it passes.
func RejectForeignOrigin(frontendBaseURL string) echo.MiddlewareFunc {
	allowed := strings.TrimSuffix(frontendBaseURL, "/")
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			switch c.Request().Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				return next(c)
			}
			if origin := c.Request().Header.Get("Origin"); origin != "" && origin != allowed {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "request from another site refused"})
			}
			return next(c)
		}
	}
}
