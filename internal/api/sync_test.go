package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func newSyncTestEcho(store userStore) *echo.Echo {
	e := echo.New()
	NewSyncHandler(store).Register(e)
	return e
}

func TestRecentEvents_RequireSignIn(t *testing.T) {
	e := newSyncTestEcho(newFakeUserStore())

	getRec := httptest.NewRecorder()
	e.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/me/recent-events", nil))
	if getRec.Code != http.StatusUnauthorized {
		t.Errorf("GET status = %d, want %d", getRec.Code, http.StatusUnauthorized)
	}

	postRec := httptest.NewRecorder()
	e.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/api/me/recent-events", strings.NewReader(`{}`)))
	if postRec.Code != http.StatusUnauthorized {
		t.Errorf("POST status = %d, want %d", postRec.Code, http.StatusUnauthorized)
	}
}

func TestRecentEvents_RecordAndList(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	record := func(eventID, eventName string, teamEvent bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"eventId": %q, "eventName": %q, "teamEvent": %v}`, eventID, eventName, teamEvent)
		req := httptest.NewRequest(http.MethodPost, "/api/me/recent-events", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	if rec := record("evt-1", "The Challengers Cup", true); rec.Code != http.StatusNoContent {
		t.Fatalf("record status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if rec := record("evt-2", "Local RTT", false); rec.Code != http.StatusNoContent {
		t.Fatalf("record status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me/recent-events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var events []recentEventResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	// Most-recently-recorded first.
	if len(events) != 2 || events[0].EventID != "evt-2" || events[1].EventID != "evt-1" {
		t.Errorf("events = %+v, want [evt-2, evt-1] (most recent first)", events)
	}
	if !events[1].TeamEvent || events[0].TeamEvent {
		t.Errorf("events = %+v, want evt-1 teamEvent=true, evt-2 teamEvent=false", events)
	}
}

func TestRecentEvents_RejectsMissingEventID(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	req := httptest.NewRequest(http.MethodPost, "/api/me/recent-events", strings.NewReader(`{"eventName": "No Id"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestRoundNote_RequiresSignIn(t *testing.T) {
	e := newSyncTestEcho(newFakeUserStore())

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me/events/evt-1/rounds/3/note"},
		{http.MethodPut, "/api/me/events/evt-1/rounds/3/note"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestRoundNote_GetIsEmptyUntilSet(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	req := httptest.NewRequest(http.MethodGet, "/api/me/events/evt-1/rounds/3/note", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body roundNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if body.Note != "" {
		t.Errorf("note = %q, want empty before ever set", body.Note)
	}
}

func TestRoundNote_SetThenGetRoundTrips(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	set := func(round, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/me/events/evt-1/rounds/"+round+"/note", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	get := func(round string) roundNoteResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/me/events/evt-1/rounds/"+round+"/note", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body roundNoteResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("couldn't parse body: %v", err)
		}
		return body
	}

	if rec := set("3", `{"note": "Remember to redeploy fliers"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if got := get("3"); got.Note != "Remember to redeploy fliers" {
		t.Errorf("note = %q, want %q", got.Note, "Remember to redeploy fliers")
	}
	// A different round on the same event is independent.
	if got := get("4"); got.Note != "" {
		t.Errorf("round 4 note = %q, want empty (independent of round 3)", got.Note)
	}

	// Overwriting replaces, not appends.
	if rec := set("3", `{"note": "Updated note"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("overwrite status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := get("3"); got.Note != "Updated note" {
		t.Errorf("note after overwrite = %q, want %q", got.Note, "Updated note")
	}

	// Setting to empty clears it back to "" rather than leaving a
	// whitespace-only row behind.
	if rec := set("3", `{"note": "   "}`); rec.Code != http.StatusNoContent {
		t.Fatalf("clear status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := get("3"); got.Note != "" {
		t.Errorf("note after clearing = %q, want empty", got.Note)
	}
}

func TestRoundNote_RejectsNonPositiveRound(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	for _, round := range []string{"0", "-1", "not-a-number"} {
		req := httptest.NewRequest(http.MethodGet, "/api/me/events/evt-1/rounds/"+round+"/note", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("round %q: status = %d, want %d (body: %s)", round, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}
}
