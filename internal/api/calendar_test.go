package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// EnsureCalendarToken/GetUserByCalendarToken extend fakeUserStore
// (defined in auth_test.go) to satisfy the fuller userStore interface
// this handler needs — same fake, same package, same pattern as
// me_test.go's SetBcpUserID.
func (f *fakeUserStore) EnsureCalendarToken(_ context.Context, userID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if token, ok := f.calendarTokens[userID]; ok && token != "" {
		return token, nil
	}
	token := fmt.Sprintf("calendar-token-%d", userID)
	f.calendarTokens[userID] = token
	return token, nil
}

func (f *fakeUserStore) GetUserByCalendarToken(_ context.Context, token string) (user.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for userID, t := range f.calendarTokens {
		if t != token {
			continue
		}
		for _, u := range f.byGoogle {
			if u.ID == userID {
				return u, nil
			}
		}
	}
	return user.User{}, user.ErrCalendarTokenNotFound
}

func newCalendarTestEcho(store userStore, client *bcp.Client, frontendURL string) *echo.Echo {
	e := echo.New()
	NewCalendarHandler(store, client, frontendURL).Register(e)
	return e
}

func TestCalendarURL_RequiresSignIn(t *testing.T) {
	e := newCalendarTestEcho(newFakeUserStore(), bcp.NewClient(), "http://frontend.example.com")
	req := httptest.NewRequest(http.MethodGet, "/api/me/calendar-url", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestCalendarURL_ReturnsAStableIcsURL(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newCalendarTestEcho(store, bcp.NewClient(), "http://frontend.example.com")

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/api/me/calendar-url", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("couldn't parse body: %v", err)
		}
		return body["url"]
	}

	first := get()
	if !strings.Contains(first, "/api/calendar/") || !strings.HasSuffix(first, ".ics") {
		t.Errorf("url = %q, want it to contain /api/calendar/ and end in .ics", first)
	}

	// A second call must return the exact same token/URL — the whole
	// point is a stable, subscribable link, not a fresh one every time.
	second := get()
	if first != second {
		t.Errorf("url changed between calls: %q vs %q, want the same token reused", first, second)
	}
}

func TestCalendarFeed_UnknownToken(t *testing.T) {
	e := newCalendarTestEcho(newFakeUserStore(), bcp.NewClient(), "http://frontend.example.com")
	req := httptest.NewRequest(http.MethodGet, "/api/calendar/does-not-exist.ics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCalendarFeed_NotLinked(t *testing.T) {
	store := newFakeUserStore()
	_, userID := signedInSession(t, store)
	token, err := store.EnsureCalendarToken(context.Background(), userID)
	if err != nil {
		t.Fatalf("EnsureCalendarToken: %v", err)
	}
	e := newCalendarTestEcho(store, bcp.NewClient(), "http://frontend.example.com")

	req := httptest.NewRequest(http.MethodGet, "/api/calendar/"+token+".ics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != icsContentType {
		t.Errorf("Content-Type = %q, want %q", ct, icsContentType)
	}
	if strings.Contains(rec.Body.String(), "BEGIN:VEVENT") {
		t.Errorf("expected an empty calendar (no linked BCP profile), got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BEGIN:VCALENDAR") {
		t.Errorf("expected a valid (if empty) VCALENDAR wrapper, got: %s", rec.Body.String())
	}
}

// stubBCPHistoryServerWithDates is like me_test.go's stubBCPHistoryServer
// but gives evt-present/evt-future real Dates — stubBCPHistoryServer's
// own fixtures leave those blank, which is fine for MeHandler.Events'
// own tests (none of them assert on StartDate/EndDate) but not for
// checking buildICS actually renders a real DTSTART. Dates are computed
// relative to time.Now() (not hardcoded) so evt-present reliably stays
// "started, not stale" (see isStaleEvent) no matter when this test runs.
func stubBCPHistoryServerWithDates(t *testing.T) *httptest.Server {
	t.Helper()
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	nextMonth := time.Now().AddDate(0, 1, 0).Format("2006-01-02")

	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"event": {"id": "evt-past", "name": "Already Placed"}},
			{"event": {"id": "evt-present", "name": "Happening Now"}},
			{"event": {"id": "evt-future", "name": "Not Started Yet"}}
		]}`))
	})
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"placing": 4, "points": 55.5, "event": {"id": "evt-past", "name": "Already Placed", "eventDate": "2024-01-01T00:00:00.000Z"}}
		]}`))
	})
	mux.HandleFunc("/events/evt-present", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id": "evt-present", "name": "Happening Now", "status": {"started": true, "ended": false}, "dates": {"start": %q, "end": %q}}`, yesterday, tomorrow)
	})
	mux.HandleFunc("/events/evt-future", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id": "evt-future", "name": "Not Started Yet", "status": {"started": false, "ended": false}, "dates": {"start": %q}}`, nextMonth)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestCalendarFeed_ValidToken(t *testing.T) {
	store := newFakeUserStore()
	_, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}
	token, err := store.EnsureCalendarToken(context.Background(), userID)
	if err != nil {
		t.Fatalf("EnsureCalendarToken: %v", err)
	}

	server := stubBCPHistoryServerWithDates(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newCalendarTestEcho(store, client, "http://frontend.example.com")

	req := httptest.NewRequest(http.MethodGet, "/api/calendar/"+token+".ics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); ct != icsContentType {
		t.Errorf("Content-Type = %q, want %q", ct, icsContentType)
	}
	// stubBCPHistoryServer's present/future events (evt-present, evt-future)
	// should be on the feed; evt-past (already concluded) should not.
	if !strings.Contains(body, "UID:evt-present@brass-ledger.app") {
		t.Errorf("expected evt-present's VEVENT, got: %s", body)
	}
	if !strings.Contains(body, "UID:evt-future@brass-ledger.app") {
		t.Errorf("expected evt-future's VEVENT, got: %s", body)
	}
	if strings.Contains(body, "evt-past") {
		t.Errorf("expected no reference to the already-concluded evt-past, got: %s", body)
	}
	if !strings.Contains(body, "URL:http://frontend.example.com/?event=evt-present") {
		t.Errorf("expected evt-present's URL to link back to this app, got: %s", body)
	}
}

func TestBuildICS(t *testing.T) {
	t.Run("empty input still produces a valid, empty calendar", func(t *testing.T) {
		out := string(buildICS(nil, nil, "http://frontend.example.com"))
		if !strings.Contains(out, "BEGIN:VCALENDAR") || !strings.Contains(out, "END:VCALENDAR") {
			t.Errorf("missing VCALENDAR wrapper: %s", out)
		}
		if strings.Contains(out, "BEGIN:VEVENT") {
			t.Errorf("expected no VEVENT for empty input: %s", out)
		}
	})

	t.Run("a multi-day event's DTEND is the day after its inclusive end date", func(t *testing.T) {
		events := []myEvent{{EventID: "evt-1", EventName: "Cup", StartDate: "2026-01-01", EndDate: "2026-01-02"}}
		out := string(buildICS(events, nil, "http://frontend.example.com"))
		if !strings.Contains(out, "DTSTART;VALUE=DATE:20260101") {
			t.Errorf("wrong/missing DTSTART: %s", out)
		}
		if !strings.Contains(out, "DTEND;VALUE=DATE:20260103") {
			t.Errorf("DTEND should be one day past the inclusive end date (20260103), got: %s", out)
		}
	})

	t.Run("a missing end date is treated as a single-day event", func(t *testing.T) {
		events := []myEvent{{EventID: "evt-1", EventName: "RTT", StartDate: "2026-03-05"}}
		out := string(buildICS(events, nil, "http://frontend.example.com"))
		if !strings.Contains(out, "DTSTART;VALUE=DATE:20260305") || !strings.Contains(out, "DTEND;VALUE=DATE:20260306") {
			t.Errorf("expected a single-day event (DTSTART 20260305, DTEND 20260306), got: %s", out)
		}
	})

	t.Run("an event with no usable start date is skipped, not left malformed", func(t *testing.T) {
		events := []myEvent{{EventID: "evt-1", EventName: "No Date"}}
		out := string(buildICS(events, nil, "http://frontend.example.com"))
		if strings.Contains(out, "BEGIN:VEVENT") {
			t.Errorf("expected no VEVENT for an event with no parseable start date: %s", out)
		}
	})

	t.Run("commas in an event name are escaped per RFC 5545", func(t *testing.T) {
		events := []myEvent{{EventID: "evt-1", EventName: "Games Workshop, Denver", StartDate: "2026-01-01"}}
		out := string(buildICS(events, nil, "http://frontend.example.com"))
		if !strings.Contains(out, `SUMMARY:Games Workshop\, Denver`) {
			t.Errorf("comma not escaped in SUMMARY: %s", out)
		}
	})

	t.Run("present and future are both included, in order", func(t *testing.T) {
		present := []myEvent{{EventID: "evt-present", EventName: "Now", StartDate: "2026-01-01"}}
		future := []myEvent{{EventID: "evt-future", EventName: "Later", StartDate: "2026-06-01"}}
		out := string(buildICS(present, future, "http://frontend.example.com"))
		if !strings.Contains(out, "UID:evt-present@brass-ledger.app") || !strings.Contains(out, "UID:evt-future@brass-ledger.app") {
			t.Errorf("expected both present and future events, got: %s", out)
		}
	})

	t.Run("an empty frontendURL omits the URL line rather than emitting a broken one", func(t *testing.T) {
		events := []myEvent{{EventID: "evt-1", EventName: "Cup", StartDate: "2026-01-01"}}
		out := string(buildICS(events, nil, ""))
		if strings.Contains(out, "URL:") {
			t.Errorf("expected no URL line when frontendURL is empty: %s", out)
		}
	})
}
