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

// SetBcpUserID extends fakeUserStore (defined in auth_test.go) to
// satisfy the fuller userStore interface these routes need — same fake,
// same package, just adding the one method auth_test.go's cases never
// exercised.
func (f *fakeUserStore) SetBcpUserID(_ context.Context, userID int64, bcpUserID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub, u := range f.byGoogle {
		if u.ID == userID {
			u.BcpUserID = bcpUserID
			f.byGoogle[sub] = u
			return nil
		}
	}
	return user.ErrSessionNotFound
}

// SetThemePreference extends fakeUserStore the same way SetBcpUserID
// above does, for MeHandler.SetTheme's tests.
func (f *fakeUserStore) SetThemePreference(_ context.Context, userID int64, theme string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub, u := range f.byGoogle {
		if u.ID == userID {
			u.ThemePreference = theme
			f.byGoogle[sub] = u
			return nil
		}
	}
	return user.ErrSessionNotFound
}

// signedInSession signs a fake user in (bypassing the Google flow
// entirely, since these tests are only about the /api/me/* routes'
// own logic) and returns a session cookie for them plus their id.
func signedInSession(t *testing.T, store *fakeUserStore) (*http.Cookie, int64) {
	t.Helper()
	u, _, err := store.UpsertUserFromGoogle(context.Background(), "sub-1", "a@example.com", "Anna Adams", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	token, err := store.CreateSession(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: token}, u.ID
}

func newMeTestEcho(store userStore, client *bcp.Client) *echo.Echo {
	e := echo.New()
	NewMeHandler(store, client).Register(e)
	return e
}

func TestBcpProfile_RequiresSignIn(t *testing.T) {
	e := newMeTestEcho(newFakeUserStore(), bcp.NewClient())
	req := httptest.NewRequest(http.MethodPost, "/api/me/bcp-profile", strings.NewReader(`{"bcpUserId": "u1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestBcpProfile_LinksAndUnlinks(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	e := newMeTestEcho(store, bcp.NewClient())

	link := func(bcpUserID string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"bcpUserId": %q}`, bcpUserID)
		req := httptest.NewRequest(http.MethodPost, "/api/me/bcp-profile", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	rec := link("  L8GE7LCQ0B  ") // leading/trailing space, as a pasted value might have
	if rec.Code != http.StatusOK {
		t.Fatalf("link status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if body["bcpUserId"] != "L8GE7LCQ0B" {
		t.Errorf("bcpUserId = %v, want trimmed L8GE7LCQ0B", body["bcpUserId"])
	}

	u, err := store.GetUserBySession(context.Background(), cookie.Value)
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if u.BcpUserID != "L8GE7LCQ0B" || u.ID != userID {
		t.Errorf("store user = %+v, want BcpUserID L8GE7LCQ0B for user %d", u, userID)
	}

	// Unlinking (empty string) works too.
	rec = link("")
	if rec.Code != http.StatusOK {
		t.Fatalf("unlink status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	u, err = store.GetUserBySession(context.Background(), cookie.Value)
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if u.BcpUserID != "" {
		t.Errorf("BcpUserID after unlinking = %q, want empty", u.BcpUserID)
	}
}

func TestSetTheme_RequiresSignIn(t *testing.T) {
	e := newMeTestEcho(newFakeUserStore(), bcp.NewClient())
	req := httptest.NewRequest(http.MethodPost, "/api/me/theme", strings.NewReader(`{"theme": "dark"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestSetTheme_DoesNotRequireApproval confirms this route uses
// requireUser, not requireApprovedUser — same as SetBcpProfile, a
// pending account should still be able to set a personal UI preference.
func TestSetTheme_DoesNotRequireApproval(t *testing.T) {
	store := newFakeUserStore()
	u, _, err := store.UpsertUserFromGoogle(context.Background(), "sub-pending", "p@example.com", "Pat Pending", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	if err := store.SetStatus(context.Background(), u.ID, user.StatusPending); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	token, err := store.CreateSession(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: token}

	e := newMeTestEcho(store, bcp.NewClient())
	req := httptest.NewRequest(http.MethodPost, "/api/me/theme", strings.NewReader(`{"theme": "dark"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
}

func TestSetTheme_SavesAndRejectsInvalidValues(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	e := newMeTestEcho(store, bcp.NewClient())

	setTheme := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/me/theme", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	rec := setTheme(`{"theme": "dark"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	u, err := store.GetUserBySession(context.Background(), cookie.Value)
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if u.ThemePreference != user.ThemeDark || u.ID != userID {
		t.Errorf("store user = %+v, want ThemePreference dark for user %d", u, userID)
	}

	for _, bad := range []string{`{"theme": "purple"}`, `{}`, `not json`} {
		rec := setTheme(bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("setTheme(%s) status = %d, want %d (body: %s)", bad, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}
}

func TestMyEvents_RequiresSignIn(t *testing.T) {
	e := newMeTestEcho(newFakeUserStore(), bcp.NewClient())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me/events", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMyEvents_NotLinked(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newMeTestEcho(store, bcp.NewClient())

	req := httptest.NewRequest(http.MethodGet, "/api/me/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp myEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if resp.Linked {
		t.Error("linked = true, want false for an account with no BCP profile set")
	}
	if len(resp.Past) != 0 || len(resp.Present) != 0 || len(resp.Future) != 0 {
		t.Errorf("expected all three sections empty, got %+v", resp)
	}
}

// stubBCPHistoryServer serves the three BCP endpoints GET /api/me/events
// needs: the registration list, the placings history, and per-event
// info for whatever isn't covered by the placings history.
func stubBCPHistoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"event": {"id": "evt-past", "name": "Already Placed"}},
			{"event": {"id": "evt-present", "name": "Happening Now"}},
			{"event": {"id": "evt-future", "name": "Not Started Yet"}},
			{"event": {"id": "evt-stale", "name": "Forgotten About"}}
		]}`))
	})
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"placing": 4, "points": 55.5, "event": {"id": "evt-past", "name": "Already Placed", "eventDate": "2024-01-01T00:00:00.000Z"}}
		]}`))
	})
	mux.HandleFunc("/events/evt-present", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-present", "name": "Happening Now", "status": {"started": true, "ended": false}}`))
	})
	mux.HandleFunc("/events/evt-future", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-future", "name": "Not Started Yet", "status": {"started": false, "ended": false}}`))
	})
	// Never marked "ended" by its organizer, but its listed end date was
	// weeks ago — TestMyEvents_ClassifiesPastPresentFuture checks this
	// still lands in Past, not stuck in Present forever (see
	// isStaleEvent).
	mux.HandleFunc("/events/evt-stale", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-stale", "name": "Forgotten About", "status": {"started": true, "ended": false}, "dates": {"start": "2020-01-01", "end": "2020-01-02"}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestMyEvents_ClassifiesPastPresentFuture(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	server := stubBCPHistoryServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newMeTestEcho(store, client)

	req := httptest.NewRequest(http.MethodGet, "/api/me/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp myEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}

	if !resp.Linked {
		t.Fatal("linked = false, want true")
	}
	// Past now legitimately holds two entries — evt-past (from the
	// placings-history endpoint, with a real placing) and evt-stale
	// (registered but never placed, reclassified out of Present purely
	// for being long past its end date with the organizer never marking
	// it ended — see isStaleEvent) — so this looks each up by id rather
	// than assuming a single entry at index 0.
	if len(resp.Past) != 2 {
		t.Fatalf("past = %+v, want exactly 2 entries (evt-past, evt-stale)", resp.Past)
	}
	var pastEvent, staleEvent *myEvent
	for i := range resp.Past {
		switch resp.Past[i].EventID {
		case "evt-past":
			pastEvent = &resp.Past[i]
		case "evt-stale":
			staleEvent = &resp.Past[i]
		}
	}
	if pastEvent == nil {
		t.Fatalf("past = %+v, want evt-past included", resp.Past)
	}
	if pastEvent.Placing == nil || *pastEvent.Placing != 4 {
		t.Errorf("evt-past.Placing = %v, want 4", pastEvent.Placing)
	}
	if staleEvent == nil {
		t.Errorf("past = %+v, want evt-stale included (started, never ended, long past its end date)", resp.Past)
	}
	if len(resp.Present) != 1 || resp.Present[0].EventID != "evt-present" {
		t.Errorf("present = %+v, want exactly evt-present (evt-stale should not be here)", resp.Present)
	}
	if len(resp.Future) != 1 || resp.Future[0].EventID != "evt-future" {
		t.Errorf("future = %+v, want exactly evt-future", resp.Future)
	}
}

// TestMyEvents_DedupesSameEventScoredUnderTwoLeagues reproduces a
// real-world case (e.g. "Cowboy Classic 2") where BCP scores one event
// under both its flagship ITC league and a separate Hobby Track league,
// which otherwise made that event show up twice in Past with two
// different point totals — confirmed live. classifyMyEvents now runs
// placingHistory through canonicalPlacingPerEvent (already proven out
// by stats_test.go) before building Past, so only the flagship entry
// should survive.
func TestMyEvents_DedupesSameEventScoredUnderTwoLeagues(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"event": {"id": "evt-dual", "name": "Cowboy Classic 2"}}]}`))
	})
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"placing": 5, "points": 60, "leagueId": "league-flagship", "event": {"id": "evt-dual", "name": "Cowboy Classic 2", "eventDate": "2025-06-01T00:00:00.000Z"}},
			{"placing": 2, "points": 90, "leagueId": "league-hobby", "event": {"id": "evt-dual", "name": "Cowboy Classic 2", "eventDate": "2025-06-01T00:00:00.000Z"}}
		]}`))
	})
	mux.HandleFunc("/leagues/league-flagship", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Flagship ITC", "gw_itc": true, "hobby": false}`))
	})
	mux.HandleFunc("/leagues/league-hobby", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Hobby Track", "gw_itc": true, "hobby": true}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newMeTestEcho(store, client)

	req := httptest.NewRequest(http.MethodGet, "/api/me/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp myEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if len(resp.Past) != 1 {
		t.Fatalf("past = %+v, want exactly 1 entry (deduped to the flagship placing)", resp.Past)
	}
	if got := resp.Past[0].Placing; got == nil || *got != 5 {
		t.Errorf("past[0].Placing = %v, want 5 (the flagship entry, not the Hobby Track one)", got)
	}
}

// TestMyEvents_UpcomingFetchedAt confirms the "last checked" timestamp
// is present and recent after a normal fetch. The Invalidate-bypasses-
// the-cache behavior ?refresh=true triggers is already thoroughly
// covered at the layer it actually lives in — bcp/cache_test.go's
// TestCache_Invalidate (the throttle/timing logic itself) and
// bcp/history_test.go's TestInvalidatePlayerEventHistory and
// bcp/events_test.go's TestInvalidateEventInfo (that the right cache key
// gets invalidated) — not retested here. What's worth confirming at this
// layer is just the wire contract: the field shows up, and ?refresh=true
// is accepted without breaking the normal response.
func TestMyEvents_UpcomingFetchedAt(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	server := stubBCPHistoryServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newMeTestEcho(store, client)

	before := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/api/me/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	after := time.Now()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp myEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}

	if resp.UpcomingFetchedAt == "" {
		t.Fatal("upcomingFetchedAt is empty, want a timestamp")
	}
	fetchedAt, err := time.Parse(time.RFC3339, resp.UpcomingFetchedAt)
	if err != nil {
		t.Fatalf("upcomingFetchedAt = %q isn't valid RFC3339: %v", resp.UpcomingFetchedAt, err)
	}
	// RFC3339 (no fractional seconds) truncates to the second, so widen
	// the window by a second on each side rather than comparing against
	// before/after's own sub-second precision.
	if fetchedAt.Before(before.Add(-time.Second)) || fetchedAt.After(after.Add(time.Second)) {
		t.Errorf("upcomingFetchedAt = %v, want between %v and %v", fetchedAt, before, after)
	}
}

func TestMyEvents_RefreshQueryParam(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	server := stubBCPHistoryServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newMeTestEcho(store, client)

	req := httptest.NewRequest(http.MethodGet, "/api/me/events?refresh=true", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp myEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	// Same classification stubBCPHistoryServer always produces — refresh
	// shouldn't change what comes back, just how it was fetched.
	if len(resp.Present) != 1 || resp.Present[0].EventID != "evt-present" {
		t.Errorf("present = %+v, want exactly evt-present", resp.Present)
	}
	if len(resp.Future) != 1 || resp.Future[0].EventID != "evt-future" {
		t.Errorf("future = %+v, want exactly evt-future", resp.Future)
	}
}

// TestMyEvents_EmptySectionsEncodeAsEmptyArrays is a regression test: an
// account linked to BCP but with zero registrations anywhere must still
// get `"past":[],"present":[],"future":[]` in the wire response, not
// `null` for whichever section(s) end up empty. Checked against the raw
// JSON bytes, not by unmarshaling into myEventsResponse — a `null` and
// an absent/empty array both unmarshal to the same nil Go slice, so that
// wouldn't have caught this. A frontend that spreads these arrays
// directly (`[...events.present, ...events.future]`, see
// brass-ledger-web's app/calendar/page.tsx) throws on `null`.
func TestMyEvents_EmptySectionsEncodeAsEmptyArrays(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": []}`))
	})
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": []}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newMeTestEcho(store, client)

	req := httptest.NewRequest(http.MethodGet, "/api/me/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, field := range []string{`"past":[]`, `"present":[]`, `"future":[]`} {
		if !strings.Contains(body, field) {
			t.Errorf("body = %s, want it to contain %s (not null)", body, field)
		}
	}
}

func TestIsStaleEvent(t *testing.T) {
	cases := []struct {
		name    string
		endDate string
		want    bool
	}{
		{"empty date can't be judged, so not stale", "", false},
		{"unparseable date can't be judged, so not stale", "not a date", false},
		{"far in the future is not stale", time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339), false},
		{"just now is not stale yet", time.Now().Format(time.RFC3339), false},
		{"within the grace period is not stale yet", time.Now().Add(-2 * 24 * time.Hour).Format(time.RFC3339), false},
		{"past the grace period is stale (RFC3339)", time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339), true},
		{"past the grace period is stale (bare date)", time.Now().Add(-10 * 24 * time.Hour).Format("2006-01-02"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStaleEvent(tc.endDate); got != tc.want {
				t.Errorf("isStaleEvent(%q) = %v, want %v", tc.endDate, got, tc.want)
			}
		})
	}
}
