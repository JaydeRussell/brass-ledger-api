package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// CalendarHandler wires up a signed-in account's calendar-feed link and
// the subscribable .ics feed itself. See CalendarURL and Feed's own doc
// comments for why the feed route is authenticated differently — by an
// unguessable token in the URL — from every other route in this
// service.
type CalendarHandler struct {
	store  userStore
	client bcpClient
	// frontendURL is used to build each VEVENT's URL, linking back to
	// this app's own event page (same /?event=<id> shape
	// app/components/myEvents/eventList.tsx uses) rather than BCP's own
	// site.
	frontendURL string
}

// NewCalendarHandler builds a CalendarHandler.
func NewCalendarHandler(store userStore, client bcpClient, frontendURL string) *CalendarHandler {
	return &CalendarHandler{
		store:       store,
		client:      client,
		frontendURL: strings.TrimSuffix(frontendURL, "/"),
	}
}

// Register wires this handler's routes onto e.
func (h *CalendarHandler) Register(e *echo.Echo) {
	e.GET("/api/me/calendar-url", h.CalendarURL)
	// Deliberately not nested under /api/me/... — that prefix means "the
	// signed-in caller's own resource via session" everywhere else in
	// this service, which would be misleading here: this route is
	// authenticated by the :token itself, never a session cookie (see
	// Feed's doc comment).
	e.GET("/api/calendar/:token", h.Feed)
}

// CalendarURL is GET /api/me/calendar-url, session-gated: returns the
// signed-in account's subscribable calendar-feed URL, generating its
// token on first request (see user.Store.EnsureCalendarToken).
func (h *CalendarHandler) CalendarURL(c echo.Context) error {
	u, err := requireUser(c, h.store)
	if err != nil {
		return err
	}
	token, err := h.store.EnsureCalendarToken(c.Request().Context(), u.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	// Built from the request itself rather than a separate "this
	// backend's own public base URL" config value — Echo's c.Scheme()
	// already honors X-Forwarded-Proto behind a reverse proxy, so the
	// request already carries everything needed to build a correct
	// externally-reachable URL.
	url := fmt.Sprintf("%s://%s/api/calendar/%s.ics", c.Scheme(), c.Request().Host, token)
	return c.JSON(http.StatusOK, map[string]string{"url": url})
}

// Feed is GET /api/calendar/:token: a subscribable iCalendar feed of the
// token's owner's upcoming (present/future) events. Unlike every other
// route in this service, this one is deliberately NOT session-gated —
// a calendar app (Google/Apple/Outlook Calendar) polls its subscribed
// URL on its own schedule with no cookie to send, so the unguessable
// token in the URL is itself the credential, the same way Google/
// iCloud's own private calendar-subscription links work. An unresolvable
// token is a plain 404 (not a session-style 401 — this was never
// session auth to begin with). An account with no BCP profile linked
// gets a valid, empty calendar back — same "not linked yet is normal,
// not an error" treatment every other route gives it — not a 404.
func (h *CalendarHandler) Feed(c echo.Context) error {
	// The route param carries the literal ".ics" suffix calendar apps
	// expect a subscribable feed URL to end in — trimmed here rather
	// than fought over in the route pattern itself.
	token := strings.TrimSuffix(c.Param("token"), ".ics")

	u, err := h.store.GetUserByCalendarToken(c.Request().Context(), token)
	if err != nil {
		return c.String(http.StatusNotFound, "calendar not found")
	}

	if u.BcpUserID == "" {
		return c.Blob(http.StatusOK, icsContentType, buildICS(nil, nil, h.frontendURL))
	}

	// No refresh param here — a calendar app's own poll schedule is the
	// only "check again," the same way Past is never refreshable on
	// /api/me/events: there's no user watching this response to ask for
	// a fresher one.
	_, present, future, err := classifyMyEvents(c.Request().Context(), h.client, u.BcpUserID, false)
	if err != nil {
		return bcpError(c, err)
	}
	return c.Blob(http.StatusOK, icsContentType, buildICS(present, future, h.frontendURL))
}

const icsContentType = "text/calendar; charset=utf-8"

// icsLineBreak is RFC 5545's required line ending — plain "\n" isn't
// spec-compliant and some calendar clients are picky about it.
const icsLineBreak = "\r\n"

// buildICS renders present and future as a minimal RFC 5545 calendar
// feed — present/future's own StartDate/EndDate (BCP's bare "YYYY-MM-DD"
// event dates, see EventInfo's doc comment) map directly to all-day
// (VALUE=DATE) VEVENTs, no timezone handling needed.
func buildICS(present, future []myEvent, frontendURL string) []byte {
	var b strings.Builder
	writeICSLine(&b, "BEGIN:VCALENDAR")
	writeICSLine(&b, "VERSION:2.0")
	writeICSLine(&b, "PRODID:-//Brass Ledger//Tournament Calendar//EN")
	writeICSLine(&b, "CALSCALE:GREGORIAN")
	writeICSLine(&b, "METHOD:PUBLISH")
	writeICSLine(&b, "X-WR-CALNAME:Brass Ledger — My Tournaments")

	now := time.Now().UTC().Format("20060102T150405Z")
	for _, ev := range append(append([]myEvent{}, present...), future...) {
		start, ok := parseBCPDate(ev.StartDate)
		if !ok {
			// No usable start date — nothing to put on a calendar.
			continue
		}
		// DTEND is exclusive for an all-day VEVENT, so a multi-day event
		// needs its end date pushed one day past its own listed
		// (inclusive) EndDate to actually span every day it covers in a
		// calendar app. A missing/unparseable EndDate is treated as a
		// single-day event (end = start + 1 day).
		end, ok := parseBCPDate(ev.EndDate)
		if !ok {
			end = start
		}
		end = end.AddDate(0, 0, 1)

		writeICSLine(&b, "BEGIN:VEVENT")
		writeICSLine(&b, "UID:"+ev.EventID+"@brass-ledger.app")
		writeICSLine(&b, "DTSTAMP:"+now)
		writeICSLine(&b, "DTSTART;VALUE=DATE:"+start.Format("20060102"))
		writeICSLine(&b, "DTEND;VALUE=DATE:"+end.Format("20060102"))
		writeICSLine(&b, "SUMMARY:"+icsEscape(ev.EventName))
		if frontendURL != "" {
			writeICSLine(&b, "URL:"+frontendURL+"/?event="+ev.EventID)
		}
		writeICSLine(&b, "END:VEVENT")
	}

	writeICSLine(&b, "END:VCALENDAR")
	return []byte(b.String())
}

// icsEscape escapes the characters RFC 5545 requires escaped in a text
// value (backslash first, so the following escapes' own backslashes
// don't get re-escaped).
func icsEscape(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`;`, `\;`,
		`,`, `\,`,
		"\n", `\n`,
	)
	return r.Replace(s)
}

// writeICSLine appends one folded, CRLF-terminated content line.
func writeICSLine(b *strings.Builder, line string) {
	b.WriteString(foldICSLine(line))
	b.WriteString(icsLineBreak)
}

// foldICSLine wraps line per RFC 5545's line-folding rule: no content
// line may exceed 75 octets, and a continuation starts with a single
// space. Real event names are unlikely to hit this today, but a feed
// that's spec-correct regardless costs little and avoids surprising a
// picky calendar client later.
//
// Splits on rune boundaries (not raw bytes) — an event name can contain
// multi-byte UTF-8 (an accented name, a non-Latin faction), and slicing
// a string by byte count can land mid-character, corrupting it. Using a
// conservative rune budget under the 75-octet limit means every chunk
// still fits within it even for 2-3-byte-per-rune text.
func foldICSLine(line string) string {
	const maxLineLen = 75
	if len(line) <= maxLineLen {
		return line
	}
	const maxRunesPerChunk = 50
	runes := []rune(line)
	var b strings.Builder
	for len(runes) > 0 {
		n := maxRunesPerChunk
		if n > len(runes) {
			n = len(runes)
		}
		if b.Len() > 0 {
			b.WriteString(icsLineBreak + " ")
		}
		b.WriteString(string(runes[:n]))
		runes = runes[n:]
	}
	return b.String()
}
