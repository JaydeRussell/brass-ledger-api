package api

import (
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// Response caching for the BCP proxy routes. Without a Cache-Control
// header every navigation re-asks for everything over the network,
// including an already-concluded event's roster, which cannot change and
// which this service serves from Postgres indefinitely.
//
// Two rules, and the second is the one that needs justifying:
//
//   - Data from a concluded event is immutable, so it gets a real
//     lifetime (EndedEventTTL).
//   - Everything else gets the interval this service itself reuses an
//     answer for (bcp.MinRefetchInterval). Within that window a repeat
//     request would mostly be served from the in-memory cache anyway, so
//     letting the browser skip it saves the trip for little extra
//     staleness. It does add some: the browser's max-age counts from
//     when it received the response, and the server's copy may already
//     have been up to one interval old then (or up to two, when served
//     stale while revalidating). So a browser can show data up to about
//     two intervals old, three in the stale-served case.
//
// Always `private`, never `public`. These responses are per-session and
// reached with credentials; a shared cache must never hold one.
//
// Never applied to /api/me/* — a signed-in account's own data is small,
// changes for reasons only that user knows about, and is exactly what
// someone hits refresh expecting to see move.
const (
	cacheControlHeader = "Cache-Control"

	// An explicit "check again now" must not be answered from any cache,
	// including the browser's. The frontend's refresh paths send
	// ?refresh=true and mean it.
	noStore = "no-store"
)

// cacheFor sets Cache-Control for a BCP-derived response.
//
// immutable reports whether this response came entirely from an event
// that has concluded. Callers pass it only when they already hold the
// answer — resolving an event's status purely to decide a cache header
// would trade a header for a BCP round trip, which is the wrong way
// round. Callers that can't know cheaply pass false and get the short
// interval, which is always correct if sometimes pessimistic.
func cacheFor(c echo.Context, immutable bool) {
	if c.QueryParam("refresh") == "true" {
		c.Response().Header().Set(cacheControlHeader, noStore)
		return
	}
	if immutable {
		c.Response().Header().Set(cacheControlHeader,
			fmt.Sprintf("private, max-age=%d, immutable", int(bcp.EndedEventTTL.Seconds())))
		return
	}
	c.Response().Header().Set(cacheControlHeader,
		fmt.Sprintf("private, max-age=%d", int(bcp.MinRefetchInterval.Seconds())))
}

// noCache marks a response as never storable — used for the routes whose
// answer is per-account and expected to move under the user's feet.
func noCache(c echo.Context) {
	c.Response().Header().Set(cacheControlHeader, noStore)
}

// DefaultNoStore marks every response no-store unless its handler calls
// cacheFor, so a route that forgets to choose never leaves caching to
// the browser's heuristics. Personal data (dossiers, friends, stats) is
// mostly what that would cover.
func DefaultNoStore(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(cacheControlHeader, noStore)
		return next(c)
	}
}
