package api

import (
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// Response caching for the BCP proxy routes.
//
// Until this existed, nothing in this service set a Cache-Control header
// at all, so every navigation re-asked for everything over the network —
// including an already-concluded event's roster, which cannot change and
// which this service will happily serve from Postgres forever. The
// browser was the one cache in the chain doing no work.
//
// Two rules, and the second is the one that needs justifying:
//
//   - Data from a concluded event is immutable, so it gets a real
//     lifetime (EndedEventTTL).
//   - Everything else gets exactly the interval this service would have
//     answered identically over anyway (bcp.MinRefetchInterval). That is
//     not a guess about staleness: within that window a second request
//     is served from the in-memory cache without touching BCP, so
//     letting the browser skip the request entirely produces the same
//     bytes with none of the cost. Nothing is served staler than it
//     already was.
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
