package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

func newSyncTestEcho(store userStore) *echo.Echo {
	e := echo.New()
	NewSyncHandler(store).Register(e)
	return e
}

// CountFollows extends fakeUserStore (defined in auth_test.go) the same
// way SetBcpUserID (me_test.go) does — aggregates across every fake
// user's own follows map (see auth_test.go's `follows` field) for the
// requested event, same grouping the real Store's SQL does.
func (f *fakeUserStore) CountFollows(_ context.Context, eventID string) ([]user.FollowCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := eventID + "|"
	byKey := make(map[string]*user.FollowCount)
	var order []string
	for _, userFollows := range f.follows {
		for key, follow := range userFollows {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			countKey := follow.Kind + ":" + follow.RefID
			c, exists := byKey[countKey]
			if !exists {
				c = &user.FollowCount{Kind: follow.Kind, RefID: follow.RefID}
				byKey[countKey] = c
				order = append(order, countKey)
			}
			c.Count++
		}
	}
	counts := make([]user.FollowCount, len(order))
	for i, k := range order {
		counts[i] = *byKey[k]
	}
	return counts, nil
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

// secondSignedInSession signs in a *different* fake account than
// signedInSession's fixed "sub-1" — needed for TestFollowCounts below to
// prove counting is across distinct accounts, not just distinct rows for
// the same one.
func secondSignedInSession(t *testing.T, store *fakeUserStore) *http.Cookie {
	t.Helper()
	u, _, err := store.UpsertUserFromGoogle(context.Background(), "sub-2", "b@example.com", "Bea Brooks", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	token, err := store.CreateSession(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: token}
}

func TestFollowCounts_RequiresSignIn(t *testing.T) {
	e := newSyncTestEcho(newFakeUserStore())
	req := httptest.NewRequest(http.MethodGet, "/api/events/evt-1/follow-counts", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestFollowCounts_AggregatesAcrossAccounts(t *testing.T) {
	store := newFakeUserStore()
	cookieA, _ := signedInSession(t, store)
	cookieB := secondSignedInSession(t, store)
	e := newSyncTestEcho(store)

	addFollow := func(cookie *http.Cookie, body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/me/events/evt-1/follows", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("AddFollow status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
		}
	}
	// Both accounts follow the same team — should count as 2. Only
	// account A follows a player, and a follow in a *different* event —
	// neither should show up in evt-1's counts.
	addFollow(cookieA, `{"kind": "team", "refId": "t1", "label": "Team One"}`)
	addFollow(cookieB, `{"kind": "team", "refId": "t1", "label": "Team One"}`)
	addFollow(cookieA, `{"kind": "player", "refId": "p1", "label": "Player One"}`)
	otherEventReq := httptest.NewRequest(http.MethodPost, "/api/me/events/evt-2/follows", strings.NewReader(`{"kind": "team", "refId": "t1", "label": "Team One"}`))
	otherEventReq.Header.Set("Content-Type", "application/json")
	otherEventReq.AddCookie(cookieA)
	otherEventRec := httptest.NewRecorder()
	e.ServeHTTP(otherEventRec, otherEventReq)
	if otherEventRec.Code != http.StatusNoContent {
		t.Fatalf("AddFollow (other event) status = %d, want %d", otherEventRec.Code, http.StatusNoContent)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/events/evt-1/follow-counts", nil)
	req.AddCookie(cookieA)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var counts map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if counts["team:t1"] != 2 {
		t.Errorf("team:t1 count = %d, want 2", counts["team:t1"])
	}
	if counts["player:p1"] != 1 {
		t.Errorf("player:p1 count = %d, want 1", counts["player:p1"])
	}
	if _, ok := counts["team:t1-evt2-should-not-leak"]; ok {
		t.Error("unexpected key leaked from a different event")
	}
	if len(counts) != 2 {
		t.Errorf("counts = %+v, want exactly 2 entries (evt-2's follow must not appear)", counts)
	}
}

func TestFollowCounts_EmptyEventReturnsEmptyObject(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newSyncTestEcho(store)

	req := httptest.NewRequest(http.MethodGet, "/api/events/evt-nobody-follows/follow-counts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("body = %s, want {}", rec.Body.String())
	}
}
