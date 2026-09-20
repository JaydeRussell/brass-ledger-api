package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
	"github.com/labstack/echo/v4"
)

// staleEventAfter is how long past its listed end date an event that BCP
// still shows as "started, not ended" gets reclassified from Present to
// Past anyway. Some organizers never flip the "ended" switch on their own
// event even once it's clearly over, which would otherwise leave it
// stuck in "Ongoing" on the frontend's My Events page forever. A day's
// grace avoids misclassifying an event that's still legitimately running
// a little long (finals dragging on, a rescheduled last round), while
// keeping the Ongoing tag from lingering too long now that the
// Present/Future registration list itself (bcp.Client's
// playerEventHistory/placingHistory caches) is cached for up to 48
// hours — see bcp.myEventsRefetchInterval's doc comment.
// maxConcurrentEventInfo bounds how many event-info lookups one request
// has in flight at once — classifyMyEvents here, and eventInfoByID in
// stats.go.
//
// Raised from three to six on 2026-09-20. The request count is
// unchanged either way; this only decides how many of them overlap. Six
// roughly halves the wait on a first-ever /stats or dossier view for an
// account with a long history, which the scale test puts in the seconds
// at 100 events and worse beyond.
//
// Two things make that affordable now. Home stopped triggering the
// per-event pass at all (?summary=true, see stats.go's withEventDetail),
// so this no longer runs on the landing page — only on a page someone
// deliberately opened. And every event resolved here is written to the
// durable cache, which is shared across every account, so each event is
// paid for once globally rather than once per user.
//
// KNOWN GAP, deliberate and the owner's call: this is a PER-REQUEST
// bound, created fresh inside each call. It says nothing about the
// process. Six simultaneous users fanning out at six put thirty-six
// concurrent requests on BCP and nothing anywhere stops them. A
// process-wide ceiling is the only thing that would actually bound what
// BCP sees; it is on the backlog, not in this change. Weigh that before
// raising this number again — past here it multiplies against traffic
// rather than adding to it.
const maxConcurrentEventInfo = 6

const staleEventAfter = 24 * time.Hour

// bcpDateLayouts are the two date shapes this service has actually seen
// from BCP: FetchEventInfo's Dates.Start/End come back as a bare date
// ("2026-01-01"), while FetchPlacingHistory's EventDate/EventEndDate come
// back full RFC3339 ("2024-01-01T00:00:00.000Z") — see internal/bcp's
// events_test.go and history_test.go fixtures for both. Tried in order;
// the first that parses wins.

// isStaleEvent reports whether an event's listed end date is far enough
// in the past that it should be treated as concluded regardless of what
// BCP's own Started/Ended flags say — see staleEventAfter's doc comment.
func isStaleEvent(endDate string) bool {
	parsed, ok := bcp.ParseDate(endDate)
	if !ok {
		return false
	}
	return time.Since(parsed) > staleEventAfter
}

// bcpProfileRequest is the body for POST /api/me/bcp-profile. The
// frontend is responsible for pulling a bare BCP user id out of
// whatever the user pastes (a profile URL or a raw id) — see that
// repo's app/lib/myEvents.ts — so this route only ever sees the id
// itself.
type bcpProfileRequest struct {
	BcpUserID string `json:"bcpUserId"`
}

// myEvent is one event in a GET /api/me/events response. The same
// shape covers all three sections: Past entries additionally carry
// Placing/Points/Faction/Team (from BCP's placings-history endpoint);
// Present/Future entries leave those empty, since BCP hasn't published
// a placing for an event that hasn't concluded yet.
type myEvent struct {
	EventID   string   `json:"eventId"`
	EventName string   `json:"eventName"`
	StartDate string   `json:"startDate,omitempty"`
	EndDate   string   `json:"endDate,omitempty"`
	Placing   *int     `json:"placing,omitempty"`
	Points    *float64 `json:"points,omitempty"`
	Faction   string   `json:"faction,omitempty"`
	Team      string   `json:"team,omitempty"`
}

// myEventsResponse is GET /api/me/events' full body. Linked is false
// (with the three sections empty) for an account that hasn't pasted a
// BCP profile yet — a normal, expected state for a new account, not an
// error — so the frontend can tell "not linked" apart from "linked but
// no events yet".
//
// UpcomingFetchedAt is when the Present/Future sections were last
// actually checked against BCP (RFC3339, omitted if never fetched this
// process's lifetime) — the registration list they're built from can go
// stale in a way only the signed-in user would know to ask about
// (registering for something new, or an event starting), so this is
// surfaced to the frontend as a "last updated" indicator alongside the
// ?refresh=true param below. Past isn't covered by this: an
// already-concluded event's placing never changes, so it has no
// meaningful staleness to report.
type myEventsResponse struct {
	Linked            bool      `json:"linked"`
	Past              []myEvent `json:"past"`
	Present           []myEvent `json:"present"`
	Future            []myEvent `json:"future"`
	UpcomingFetchedAt string    `json:"upcomingFetchedAt,omitempty"`
}

func emptyMyEventsResponse(linked bool) myEventsResponse {
	return myEventsResponse{
		Linked:  linked,
		Past:    []myEvent{},
		Present: []myEvent{},
		Future:  []myEvent{},
	}
}

// MeHandler wires up the signed-in user's own BCP-profile link and "my
// events" (past/present/future) endpoints — see the design note in
// internal/bcp/history.go's "Per-user event history" section for why
// classification needs two BCP endpoints plus a per-event fallback
// call, and CLAUDE.md's scope note: this only ever displays BCP's own
// already-published data, same as everything else in this service.
type MeHandler struct {
	store  userStore
	client bcpClient
}

// NewMeHandler builds a MeHandler.
func NewMeHandler(store userStore, client bcpClient) *MeHandler {
	return &MeHandler{store: store, client: client}
}

// Register wires this handler's routes onto e.
func (h *MeHandler) Register(e *echo.Echo) {
	e.POST("/api/me/bcp-profile", h.SetBcpProfile)
	e.GET("/api/me/events", h.Events)
	e.POST("/api/me/accent-theme", h.SetAccentTheme)
	e.POST("/api/me/dossier-visibility", h.SetDossierVisibility)
}

// SetBcpProfile is POST /api/me/bcp-profile: links (or unlinks) a BCP
// profile to the signed-in account. Deliberately requireUser, not
// requireApprovedUser — this is exactly what /welcome's onboarding flow
// calls (see the frontend's BcpProfileLinker), and a pending account
// should still be able to finish that step while waiting on approval
// rather than getting stuck before an admin's even seen them.
func (h *MeHandler) SetBcpProfile(c echo.Context) error {
	u, err := requireUser(c, h.store)
	if err != nil {
		return err
	}

	var req bcpProfileRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	bcpUserID := strings.TrimSpace(req.BcpUserID)

	if err := h.store.SetBcpUserID(c.Request().Context(), u.ID, bcpUserID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]any{"bcpUserId": bcpUserID})
}

// accentThemeRequest is the body for POST /api/me/accent-theme.
type accentThemeRequest struct {
	AccentTheme string `json:"accentTheme"`
}

// SetAccentTheme is POST /api/me/accent-theme: saves the signed-in
// account's accent-color theme choice (see brass-ledger-web's
// app/lib/theme.ts's AccentTheme/useAccentTheme). Deliberately
// requireUser, not requireApprovedUser — same reasoning as SetBcpProfile
// above: a personal UI preference, not BCP data access, so there's no
// reason to make a pending account wait on approval before they can pick
// one.
func (h *MeHandler) SetAccentTheme(c echo.Context) error {
	u, err := requireUser(c, h.store)
	if err != nil {
		return err
	}

	var req accentThemeRequest
	if bindErr := c.Bind(&req); bindErr != nil || !user.IsValidAccentTheme(req.AccentTheme) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": `"accentTheme" must be one of ` + strings.Join(user.ValidAccentThemes, ", ")})
	}

	if err := h.store.SetAccentTheme(c.Request().Context(), u.ID, req.AccentTheme); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}

// dossierVisibilityRequest is the body for POST /api/me/dossier-visibility.
type dossierVisibilityRequest struct {
	Public bool `json:"public"`
}

// SetDossierVisibility is POST /api/me/dossier-visibility: turns the
// signed-in account's public player dossier (migration 0014, see
// dossier.go) on or off. Deliberately requireUser, not
// requireApprovedUser — same reasoning as SetAccentTheme above: a
// personal privacy preference, not BCP data access, so a pending account
// isn't blocked from turning this off before an admin's even looked at
// them. (GET /api/players/:bcpUserId/dossier itself additionally
// requires the account be approved before showing anything, regardless
// of this flag — see dossier.go.)
func (h *MeHandler) SetDossierVisibility(c echo.Context) error {
	u, err := requireUser(c, h.store)
	if err != nil {
		return err
	}

	var req dossierVisibilityRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	if err := h.store.SetDossierPublic(c.Request().Context(), u.ID, req.Public); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}

// Events is GET /api/me/events: the signed-in account's BCP events,
// classified into past/present/future. requireApprovedUser, not
// requireUser — unlike SetBcpProfile above, this is real BCP data, the
// same "the whole app needs approval, not just sign-in" bar as
// BCPHandler's routes.
func (h *MeHandler) Events(c echo.Context) error {
	noCache(c)
	u, err := requireApprovedUser(c, h.store)
	if err != nil {
		return err
	}

	if u.BcpUserID == "" {
		return c.JSON(http.StatusOK, emptyMyEventsResponse(false))
	}

	ctx := c.Request().Context()

	// An explicit "check again now" request — see
	// bcp.Cache.Invalidate's doc comment for why this is scoped to
	// just the registration list (and, below, each not-yet-concluded
	// event's own info) rather than also forcing FetchPlacingHistory
	// to bypass its cache: an already-concluded event's placing can
	// never change, so there's nothing there a refresh could
	// meaningfully improve — only Present/Future can go stale in a
	// way the user would know to ask about.
	refresh := c.QueryParam("refresh") == "true"
	past, present, future, err := classifyMyEvents(ctx, h.client, u.BcpUserID, refresh)
	if err != nil {
		return bcpError(c, err)
	}

	resp := emptyMyEventsResponse(true)
	resp.Past, resp.Present, resp.Future = past, present, future

	if fetchedAt, ok := h.client.PlayerEventHistoryFetchedAt(u.BcpUserID); ok {
		resp.UpcomingFetchedAt = fetchedAt.Format(time.RFC3339)
	}

	return c.JSON(http.StatusOK, resp)
}

// classifyMyEvents fetches bcpUserID's placing history and registration
// list and classifies them into past/present/future — factored out of
// MeHandler.Events as its own fetch/classify step, separate from
// building the HTTP response around it. refresh mirrors
// MeHandler.Events' ?refresh=true (see its doc comment above for why
// this bypasses the registration-list/per-event cache but never
// FetchPlacingHistory).
//
// The three return slices start non-nil (empty, not nil) rather than as
// the named-return zero value — a section that ends up with zero
// entries (e.g. no events currently Present) must still encode as JSON
// `[]`, not `null`: the frontend spreads it directly
// (`[...events.present, ...events.future]`, see app/calendar/page.tsx),
// which throws on null.
func classifyMyEvents(ctx context.Context, client bcpClient, bcpUserID string, refresh bool) (past, present, future []myEvent, err error) {
	past, present, future = []myEvent{}, []myEvent{}, []myEvent{}

	if refresh {
		client.InvalidatePlayerEventHistory(bcpUserID)
	}

	// The two feeds are independent — neither reads the other's result —
	// but each is a paginated crawl of up to maxHistoryPages sequential
	// BCP requests, so running them back to back doubled the wait for no
	// reason. Concurrently, this endpoint costs the slower of the two
	// rather than their sum.
	//
	// Not more requests to BCP, just not serialised: the same two calls,
	// overlapped. Both go through internal/bcp's cache, which dedupes
	// concurrent callers for the same key, so a simultaneous
	// /api/me/stats still shares this one crawl rather than starting
	// its own.
	var (
		placingHistory []bcp.PlacingHistoryEntry
		registrations  []bcp.PlayerEventRecord
		placingErr     error
		regErr         error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		placingHistory, placingErr = client.FetchPlacingHistory(ctx, bcpUserID)
	}()
	go func() {
		defer wg.Done()
		registrations, regErr = client.FetchPlayerEventHistory(ctx, bcpUserID)
	}()
	wg.Wait()
	if placingErr != nil {
		return nil, nil, nil, placingErr
	}
	if regErr != nil {
		return nil, nil, nil, regErr
	}

	// BCP can score one event under several leagues at once (flagship ITC
	// plus a separate Hobby Track, say), which would otherwise show up as
	// the same event listed twice in Past with two different point
	// totals — see canonicalPlacingPerEvent's doc comment (stats.go),
	// which this reuses rather than duplicating.
	client.PrewarmLeagueInfo(ctx, distinctLeagueIDs(placingHistory))
	placingHistory = canonicalPlacingPerEvent(ctx, client, placingHistory)

	concluded := make(map[string]bool, len(placingHistory))
	for _, p := range placingHistory {
		concluded[p.EventID] = true
		past = append(past, myEvent{
			EventID:   p.EventID,
			EventName: p.EventName,
			StartDate: p.EventDate,
			EndDate:   p.EventEndDate,
			Placing:   p.Placing,
			Points:    p.Points,
			Faction:   p.Faction,
			Team:      p.Team,
		})
	}

	// Anything registered but not yet in the placings history is the
	// small number of current/upcoming events (see the design note on
	// MeHandler above) — each needs its own FetchEventInfo call to learn
	// Started/Ended, since the registration list alone doesn't carry
	// dates.
	//
	// Most of these can't be durably cached at all (only ended events
	// are, see internal/bcp/events.go), so prewarming looks pointless —
	// except for the one case that isn't upcoming: an event that has
	// ended but whose placings BCP hasn't published yet, which lands
	// here rather than in placingHistory and is exactly the kind that
	// sits around for days. Misses cost nothing beyond the one query the
	// hits already pay for.
	pending := make([]string, 0, len(registrations))
	for _, r := range registrations {
		if !concluded[r.EventID] {
			pending = append(pending, r.EventID)
		}
	}
	if !refresh {
		// A refresh is an explicit "ignore what's cached", so seeding
		// from the durable cache would just be undone by the
		// invalidations below.
		client.PrewarmEventInfo(ctx, pending)
	}

	if refresh {
		for _, id := range pending {
			client.InvalidateEventInfo(id)
		}
	}

	// Resolve the pending events a few at a time rather than one after
	// another. Each is its own FetchEventInfo, and the ones that aren't
	// durably cached (anything not yet ended) are a real BCP round trip
	// — so for an account with a handful of registrations awaiting
	// results this was several hundred milliseconds each, in series, and
	// it was the single biggest remaining cost on this endpoint.
	//
	// Bounded deliberately, and kept low: this is an unofficial API we
	// have no agreement with (see CLAUDE.md). The request count is
	// unchanged — the same events, resolved the same number of times —
	// and overlapping them actually shortens the window BCP is holding
	// connections open for us. What the bound prevents is an account
	// with a long history turning one page load into a burst.
	infos := make([]bcp.EventInfo, len(pending))
	ok := make([]bool, len(pending))
	sem := make(chan struct{}, maxConcurrentEventInfo)
	var infoWg sync.WaitGroup
	for i, id := range pending {
		infoWg.Add(1)
		go func() {
			defer infoWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := client.FetchEventInfo(ctx, id)
			if err != nil {
				// One event's metadata failing to load shouldn't take
				// down the whole list — skip just that event.
				return
			}
			infos[i], ok[i] = info, true
		}()
	}
	infoWg.Wait()

	// Classified in `pending` order, not completion order, so the
	// response is stable regardless of which fetch finished first.
	for i := range pending {
		if !ok[i] {
			continue
		}
		info := infos[i]
		ev := myEvent{EventID: info.ID, EventName: info.Name, StartDate: info.StartDate, EndDate: info.EndDate}
		switch {
		case info.Started && !info.Ended && !isStaleEvent(info.EndDate):
			present = append(present, ev)
		case !info.Started:
			future = append(future, ev)
		default:
			// Either started and ended, or started-and-unended-but-
			// stale (see isStaleEvent) — either way BCP hasn't
			// published this user's placing yet (results still being
			// finalized, or the organizer never will), so the closest
			// bucket is Past even without placing details.
			past = append(past, ev)
		}
	}

	return past, present, future, nil
}
