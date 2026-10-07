package api

import (
	"net/http"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// searchMaxQueryLength stops an arbitrarily long string being passed on
// to BCP.
const searchMaxQueryLength = 100

// SearchHandler serves event search by name.
type SearchHandler struct {
	client bcpClient
}

// NewSearchHandler builds a SearchHandler.
func NewSearchHandler(client bcpClient) *SearchHandler {
	return &SearchHandler{client: client}
}

// Register wires the search route onto e. Each new query costs one BCP
// request, so it takes rateLimit as well as requireApproved.
func (h *SearchHandler) Register(e *echo.Echo, requireApproved, rateLimit echo.MiddlewareFunc) {
	e.GET("/api/event-search", h.Search, requireApproved, rateLimit)
}

// Search is GET /api/event-search?q=. The query isn't stored against the
// caller, and BCP request URLs have it redacted before they reach a log.
func (h *SearchHandler) Search(c echo.Context) error {
	q := bcp.NormalizeSearchQuery(c.QueryParam("q"))
	n := utf8.RuneCountInString(q)
	if n < bcp.SearchMinQueryLength || n > searchMaxQueryLength {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Search for 3 to 100 characters of an event's name."})
	}
	results, err := h.client.SearchEvents(c.Request().Context(), q)
	if err != nil {
		return bcpError(c, err)
	}
	cacheFor(c, false)
	return c.JSON(http.StatusOK, results)
}
