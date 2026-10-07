package api

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/geocode"
)

// searchMaxQueryLength and searchMaxCursorLength stop an arbitrarily long
// string being passed on to BCP or OpenStreetMap.
const (
	searchMaxQueryLength  = 100
	searchMaxCursorLength = 2048
	// A date range longer than this is refused rather than paged through.
	searchMaxSpan = 366 * 24 * time.Hour
	// The radius a location search uses when none is given.
	searchDefaultRadius = 50
)

type placeLookup interface {
	Search(ctx context.Context, q string) ([]geocode.Place, error)
}

// SearchHandler serves event search and the place lookup behind its
// location filter.
type SearchHandler struct {
	client bcpClient
	places placeLookup
}

// NewSearchHandler builds a SearchHandler.
func NewSearchHandler(client bcpClient, places placeLookup) *SearchHandler {
	return &SearchHandler{client: client, places: places}
}

// Register wires the search routes onto e. Each new query costs one
// request to BCP or OpenStreetMap, so both take rateLimit as well as
// requireApproved.
func (h *SearchHandler) Register(e *echo.Echo, requireApproved, rateLimit echo.MiddlewareFunc) {
	e.GET("/api/event-search", h.Search, requireApproved, rateLimit)
	e.GET("/api/places", h.Places, requireApproved, rateLimit)
}

func badSearch(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, map[string]string{"error": msg})
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// searchParams reads and checks GET /api/event-search's filters. The
// message is set when they're refused.
func searchParams(c echo.Context) (bcp.EventSearchParams, string) {
	var p bcp.EventSearchParams
	p.Query = bcp.NormalizeSearchQuery(c.QueryParam("q"))
	if n := utf8.RuneCountInString(p.Query); n > 0 && (n < bcp.SearchMinQueryLength || n > searchMaxQueryLength) {
		return p, "Search for 3 to 100 characters of an event's name."
	}

	latRaw, lonRaw := c.QueryParam("lat"), c.QueryParam("lon")
	if latRaw != "" || lonRaw != "" {
		lat, errLat := strconv.ParseFloat(latRaw, 64)
		lon, errLon := strconv.ParseFloat(lonRaw, 64)
		if errLat != nil || errLon != nil || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
			return p, "That location isn't valid."
		}
		radius := searchDefaultRadius
		if r := c.QueryParam("radius"); r != "" {
			var err error
			if radius, err = strconv.Atoi(r); err != nil || !slices.Contains(bcp.SearchRadiiMiles, radius) {
				return p, "Choose a radius of 25, 50, 100 or 250 miles."
			}
		}
		p.Near = &bcp.EventSearchNear{Lat: round2(lat), Lon: round2(lon), RadiusMiles: radius}
	}

	if p.Query == "" && p.Near == nil {
		return p, "Search by an event's name, a location, or both."
	}

	var from, to time.Time
	for _, d := range []struct {
		raw string
		str *string
		t   *time.Time
	}{{c.QueryParam("from"), &p.From, &from}, {c.QueryParam("to"), &p.To, &to}} {
		if d.raw == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", d.raw)
		if err != nil {
			return p, "Dates must look like 2026-10-31."
		}
		*d.str, *d.t = d.raw, t
	}
	if !from.IsZero() && !to.IsZero() && (to.Before(from) || to.Sub(from) > searchMaxSpan) {
		return p, "Choose an end date on or after the start date, within a year of it."
	}
	return p, ""
}

// Search is GET /api/event-search?q=&lat=&lon=&radius=&from=&to=&cursor=,
// one page at a time. Nothing about the search is stored against the
// caller, and BCP request URLs have the query and location redacted
// before they reach a log.
func (h *SearchHandler) Search(c echo.Context) error {
	p, msg := searchParams(c)
	if msg != "" {
		return badSearch(c, msg)
	}
	cursor := c.QueryParam("cursor")
	if len(cursor) > searchMaxCursorLength {
		return badSearch(c, "invalid cursor")
	}
	page, err := h.client.SearchEvents(c.Request().Context(), p, cursor)
	if err != nil {
		return bcpError(c, err)
	}
	if page.Results == nil {
		page.Results = []bcp.EventSearchResult{}
	}
	cacheFor(c, false)
	return c.JSON(http.StatusOK, page)
}

// Places is GET /api/places?q=: up to five places matching a typed name,
// for the location filter. The typed text goes to OpenStreetMap from this
// server and isn't logged or stored against the caller.
func (h *SearchHandler) Places(c echo.Context) error {
	q := geocode.Normalize(c.QueryParam("q"))
	if n := utf8.RuneCountInString(q); n < 2 || n > searchMaxQueryLength {
		return badSearch(c, "Type 2 to 100 characters of a place name.")
	}
	places, err := h.places.Search(c.Request().Context(), q)
	if err != nil {
		// geocode's errors carry no query text, so this logs none.
		log.Printf("%s %s: place lookup: %v", c.Request().Method, c.Path(), err)
		msg := "Couldn't look that place up just now. Please try again in a moment."
		if !errors.Is(err, geocode.ErrUnavailable) {
			msg = internalErrorMessage
		}
		return c.JSON(http.StatusBadGateway, map[string]string{"error": msg})
	}
	if places == nil {
		places = []geocode.Place{}
	}
	cacheFor(c, false)
	return c.JSON(http.StatusOK, places)
}
