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

func TestFollows_RequireSignIn(t *testing.T) {
	e := newSyncTestEcho(newFakeUserStore())

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me/events/evt-1/follows"},
		{http.MethodPost, "/api/me/events/evt-1/follows"},
		{http.MethodDelete, "/api/me/events/evt-1/follows/team/t1"},
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

func TestFollows_AddListRemove(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	add := func(eventID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/me/events/"+eventID+"/follows", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	list := func(eventID string) []followResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/me/events/"+eventID+"/follows", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		var follows []followResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &follows); err != nil {
			t.Fatalf("couldn't parse body: %v", err)
		}
		return follows
	}

	if rec := add("evt-1", `{"kind": "team", "refId": "t1", "label": "Team Ultramarines"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if rec := add("evt-1", `{"kind": "player", "refId": "p1", "label": "Guilliman"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	// A follow in a different event shouldn't show up when listing evt-1.
	if rec := add("evt-2", `{"kind": "team", "refId": "t9", "label": "Other Event Team"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	follows := list("evt-1")
	if len(follows) != 2 {
		t.Fatalf("evt-1 follows = %+v, want 2 entries", follows)
	}

	// Re-adding the same kind+refId updates the label rather than
	// duplicating the entry (AddFollow is idempotent).
	if rec := add("evt-1", `{"kind": "team", "refId": "t1", "label": "Renamed Team"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("re-add status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	follows = list("evt-1")
	if len(follows) != 2 {
		t.Fatalf("evt-1 follows after re-add = %+v, want still 2 entries", follows)
	}
	var team *followResponse
	for i := range follows {
		if follows[i].Kind == "team" && follows[i].RefID == "t1" {
			team = &follows[i]
		}
	}
	if team == nil || team.Label != "Renamed Team" {
		t.Errorf("team follow = %+v, want label Renamed Team", team)
	}

	// Removing one leaves the other.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/me/events/evt-1/follows/player/p1", nil)
	delReq.AddCookie(cookie)
	delRec := httptest.NewRecorder()
	e.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d (body: %s)", delRec.Code, http.StatusNoContent, delRec.Body.String())
	}
	follows = list("evt-1")
	if len(follows) != 1 || follows[0].Kind != "team" {
		t.Errorf("evt-1 follows after removing player = %+v, want just the team follow", follows)
	}
}

func TestFollows_RejectsInvalidKind(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	req := httptest.NewRequest(http.MethodPost, "/api/me/events/evt-1/follows", strings.NewReader(`{"kind": "coach", "refId": "c1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
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
