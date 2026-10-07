package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/follow"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// fakeFollowStore is an in-memory followStore.
type fakeFollowStore struct {
	mu         sync.Mutex
	nextID     int64
	links      map[int64]follow.Link
	spectating map[string]follow.Spectated // key: userID/eventID
}

func newFakeFollowStore() *fakeFollowStore {
	return &fakeFollowStore{links: map[int64]follow.Link{}, spectating: map[string]follow.Spectated{}}
}

func spKey(userID int64, eventID string) string { return fmt.Sprintf("%d/%s", userID, eventID) }

func (f *fakeFollowStore) EnsureLink(_ context.Context, userID int64, eventID, playerID string, expiresAt time.Time) (follow.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, l := range f.links {
		if l.UserID == userID && l.EventID == eventID {
			l.PlayerID, l.ExpiresAt = playerID, expiresAt
			f.links[id] = l
			return l, nil
		}
	}
	f.nextID++
	l := follow.Link{ID: f.nextID, Token: fmt.Sprintf("tok-%d", f.nextID), UserID: userID, EventID: eventID, PlayerID: playerID, ExpiresAt: expiresAt}
	f.links[l.ID] = l
	return l, nil
}

func (f *fakeFollowStore) GetLinkForUser(_ context.Context, userID int64, eventID string) (follow.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.links {
		if l.UserID == userID && l.EventID == eventID && l.ExpiresAt.After(time.Now()) {
			return l, nil
		}
	}
	return follow.Link{}, follow.ErrNotFound
}

func (f *fakeFollowStore) GetLinkByToken(_ context.Context, token string) (follow.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.links {
		if l.Token == token && l.ExpiresAt.After(time.Now()) {
			return l, nil
		}
	}
	return follow.Link{}, follow.ErrNotFound
}

func (f *fakeFollowStore) DeleteLink(_ context.Context, userID int64, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, l := range f.links {
		if l.UserID == userID && l.EventID == eventID {
			delete(f.links, id)
			for k, sp := range f.spectating {
				if sp.FollowLinkID != nil && *sp.FollowLinkID == id {
					delete(f.spectating, k)
				}
			}
		}
	}
	return nil
}

func (f *fakeFollowStore) SaveSpectating(_ context.Context, userID int64, sp follow.Spectated) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spectating[spKey(userID, sp.EventID)] = sp
	return nil
}

func (f *fakeFollowStore) ListSpectating(_ context.Context, userID int64) ([]follow.Spectated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []follow.Spectated
	for k, sp := range f.spectating {
		if strings.HasPrefix(k, fmt.Sprintf("%d/", userID)) && sp.ExpiresAt.After(time.Now()) {
			out = append(out, sp)
		}
	}
	return out, nil
}

func (f *fakeFollowStore) GetSpectating(_ context.Context, userID int64, eventID string) (follow.Spectated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp, ok := f.spectating[spKey(userID, eventID)]
	if !ok || !sp.ExpiresAt.After(time.Now()) {
		return follow.Spectated{}, follow.ErrNotFound
	}
	return sp, nil
}

func (f *fakeFollowStore) DeleteSpectating(_ context.Context, userID int64, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.spectating, spKey(userID, eventID))
	return nil
}

// followBCPServer serves two events: evt-live (started, scored under
// league-1) and evt-soon (not started). Both rosters hold player p1
// (BCP user u-owner) and p2 (u-other).
func followBCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	event := func(id string, started bool) {
		mux.HandleFunc("/events/"+id, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id": %q, "name": "Cup %s", "status": {"started": %t}, "dates": {"start": "2026-10-10", "end": "2026-10-11"}, "leagues": [{"id": "league-1", "name": "ITC"}]}`, id, id, started)
		})
		mux.HandleFunc("/events/"+id+"/players", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"active": [
				{"id": "p1", "user": {"id": "u-owner", "firstName": "Olive", "lastName": "Owner"}},
				{"id": "p2", "user": {"id": "u-other", "firstName": "Oscar", "lastName": "Other"}}
			]}`))
		})
		mux.HandleFunc("/events/"+id+"/teamplayers", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
	}
	event("evt-live", true)
	event("evt-soon", false)
	mux.HandleFunc("/events/evt-old", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-old", "name": "Old Cup", "status": {"started": true, "ended": true}, "dates": {"start": "2025-01-01", "end": "2025-01-02"}}`))
	})
	mux.HandleFunc("/events/evt-old/players", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active": [{"id": "p1", "user": {"id": "u-owner", "firstName": "Olive", "lastName": "Owner"}}]}`))
	})
	mux.HandleFunc("/events/evt-old/teamplayers", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/placings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"userId": "u-other", "ITCPoints": 100}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

type followFixture struct {
	e      *echo.Echo
	store  *fakeFollowStore
	users  *fakeUserStore
	client *bcp.Client
}

func newFollowFixture(t *testing.T) followFixture {
	t.Helper()
	users := newFakeUserStore()
	links := newFakeFollowStore()
	client := bcp.NewClientWithBaseURL(followBCPServer(t).URL)
	passThrough := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	access := NewFollowAccess(links, client, passThrough)

	e := echo.New()
	NewBCPHandler(client).Register(e, access.Or(RequireApproved(users)), access.Or(RequireSession(users)))
	NewFollowHandler(users, links, client, access).Register(e, passThrough)
	return followFixture{e: e, store: links, users: users, client: client}
}

// signIn creates an approved user, optionally linked to a BCP profile.
func (fx followFixture) signIn(t *testing.T, name, bcpUserID string) (*http.Cookie, int64) {
	t.Helper()
	cookie, id := newSignedInUser(t, fx.users, name, user.RoleUser, user.StatusApproved)
	if bcpUserID != "" {
		if err := fx.users.SetBcpUserID(context.Background(), id, bcpUserID); err != nil {
			t.Fatalf("SetBcpUserID: %v", err)
		}
	}
	return cookie, id
}

func (fx followFixture) do(method, path, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	fx.e.ServeHTTP(rec, req)
	return rec
}

func (fx followFixture) createLink(t *testing.T, cookie *http.Cookie, eventID string) string {
	t.Helper()
	rec := fx.do(http.MethodPut, "/api/events/"+eventID+"/follow-link", "", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create link: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp followLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		t.Fatalf("create link: body %s", rec.Body.String())
	}
	return resp.Token
}

func TestFollowLink_CreateRequiresRosterEntry(t *testing.T) {
	fx := newFollowFixture(t)
	pendingCookie, _ := newSignedInUser(t, fx.users, "pending", user.RoleUser, user.StatusPending)
	unlinkedCookie, _ := fx.signIn(t, "unlinked", "")
	strangerCookie, _ := fx.signIn(t, "stranger", "u-stranger")

	cases := []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"signed out", nil, http.StatusUnauthorized},
		{"pending account", pendingCookie, http.StatusForbidden},
		{"no BCP profile", unlinkedCookie, http.StatusForbidden},
		{"not on the roster", strangerCookie, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := fx.do(http.MethodPut, "/api/events/evt-live/follow-link", "", tc.cookie, nil)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestFollowLink_CreateIsIdempotentAndResolves(t *testing.T) {
	fx := newFollowFixture(t)
	owner, _ := fx.signIn(t, "owner", "u-owner")

	token := fx.createLink(t, owner, "evt-live")
	if again := fx.createLink(t, owner, "evt-live"); again != token {
		t.Errorf("second create returned %q, want the same token %q", again, token)
	}

	rec := fx.do(http.MethodGet, "/api/follow/"+token, "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: status %d", rec.Code)
	}
	var got resolvedFollowLink
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.EventID != "evt-live" || got.PlayerID != "p1" {
		t.Errorf("resolved = %+v, want evt-live/p1", got)
	}

	if rec := fx.do(http.MethodGet, "/api/follow/nope", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: status %d, want 404", rec.Code)
	}
}

func TestFollowLink_ExpiresAWeekAfterTheEvent(t *testing.T) {
	got := followExpiry(bcp.EventInfo{StartDate: "2026-10-10", EndDate: "2026-10-11"}, time.Now())
	want := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("expiry = %v, want %v", got, want)
	}
	now := time.Now()
	if got := followExpiry(bcp.EventInfo{}, now); !got.Equal(now.Add(followUndatedLifetime)) {
		t.Errorf("undated expiry = %v, want now + %v", got, followUndatedLifetime)
	}
}

func TestFollowAccess_TokenOpensOnlyItsOwnEvent(t *testing.T) {
	fx := newFollowFixture(t)
	owner, _ := fx.signIn(t, "owner", "u-owner")
	token := fx.createLink(t, owner, "evt-live")
	withToken := map[string]string{followTokenHeader: token}

	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"event info with token", "/api/events/evt-live", withToken, http.StatusOK},
		{"roster with token", "/api/events/evt-live/players", withToken, http.StatusOK},
		{"league with token", "/api/itc/leagues/event/evt-live", withToken, http.StatusOK},
		{"event info without token", "/api/events/evt-live", nil, http.StatusUnauthorized},
		{"another event with token", "/api/events/evt-soon", withToken, http.StatusUnauthorized},
		{"bad token", "/api/events/evt-live", map[string]string{followTokenHeader: "forged"}, http.StatusUnauthorized},
		{"ranking for this event's roster", "/api/itc/rankings?eventId=evt-live&leagueId=league-1&userId=u-other", withToken, http.StatusOK},
		{"ranking for a player not on the roster", "/api/itc/rankings?eventId=evt-live&leagueId=league-1&userId=u-stranger", withToken, http.StatusUnauthorized},
		{"ranking in another league", "/api/itc/rankings?eventId=evt-live&leagueId=league-2&userId=u-other", withToken, http.StatusUnauthorized},
		{"ranking without an event", "/api/itc/rankings?leagueId=league-1&userId=u-other", withToken, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := fx.do(http.MethodGet, tc.path, "", nil, tc.headers)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestFollowAccess_RevokingTakesEffectAtOnce(t *testing.T) {
	fx := newFollowFixture(t)
	owner, _ := fx.signIn(t, "owner", "u-owner")
	token := fx.createLink(t, owner, "evt-live")
	withToken := map[string]string{followTokenHeader: token}

	if rec := fx.do(http.MethodGet, "/api/events/evt-live", "", nil, withToken); rec.Code != http.StatusOK {
		t.Fatalf("before revoking: status %d", rec.Code)
	}
	if rec := fx.do(http.MethodDelete, "/api/events/evt-live/follow-link", "", owner, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d", rec.Code)
	}
	if rec := fx.do(http.MethodGet, "/api/events/evt-live", "", nil, withToken); rec.Code != http.StatusUnauthorized {
		t.Errorf("after revoking: status %d, want 401", rec.Code)
	}
	if rec := fx.do(http.MethodGet, "/api/follow/"+token, "", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("resolve after revoking: status %d, want 404", rec.Code)
	}
}

func saveSpectating(t *testing.T, fx followFixture, cookie *http.Cookie, body string) (int, bool) {
	t.Helper()
	rec := fx.do(http.MethodPost, "/api/me/spectating", body, cookie, nil)
	var resp saveSpectatingResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.Saved
}

func TestSpectating_SaveRules(t *testing.T) {
	fx := newFollowFixture(t)
	owner, _ := fx.signIn(t, "owner", "u-owner")
	other, _ := fx.signIn(t, "other", "u-other")
	fan, _ := fx.signIn(t, "fan", "")
	token := fx.createLink(t, owner, "evt-live")

	cases := []struct {
		name      string
		cookie    *http.Cookie
		body      string
		wantCode  int
		wantSaved bool
	}{
		{"own link", owner, `{"token": "` + token + `"}`, http.StatusOK, false},
		{"already on the roster", other, `{"token": "` + token + `"}`, http.StatusOK, false},
		{"fan through a link", fan, `{"token": "` + token + `"}`, http.StatusOK, true},
		{"fan picking a player", fan, `{"eventId": "evt-soon", "playerId": "p2"}`, http.StatusOK, true},
		{"player not on the roster", fan, `{"eventId": "evt-soon", "playerId": "p9"}`, http.StatusNotFound, false},
		{"dead token", fan, `{"token": "forged"}`, http.StatusNotFound, false},
		{"nothing to follow", fan, `{}`, http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, saved := saveSpectating(t, fx, tc.cookie, tc.body)
			if code != tc.wantCode || saved != tc.wantSaved {
				t.Errorf("got %d saved=%t, want %d saved=%t", code, saved, tc.wantCode, tc.wantSaved)
			}
		})
	}
}

func TestSpectating_ListSplitsNowAndUpcomingAndCascades(t *testing.T) {
	fx := newFollowFixture(t)
	owner, _ := fx.signIn(t, "owner", "u-owner")
	fan, fanID := fx.signIn(t, "fan", "")
	token := fx.createLink(t, owner, "evt-live")
	saveSpectating(t, fx, fan, `{"token": "`+token+`"}`)
	saveSpectating(t, fx, fan, `{"eventId": "evt-soon", "playerId": "p2"}`)

	list := func() spectatingListResponse {
		rec := fx.do(http.MethodGet, "/api/me/spectating", "", fan, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: status %d", rec.Code)
		}
		var resp spectatingListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp
	}

	got := list()
	if len(got.Now) != 1 || got.Now[0].EventID != "evt-live" || got.Now[0].PlayerName != "Olive Owner" || !got.Now[0].ViaLink {
		t.Errorf("now = %+v, want evt-live following Olive Owner via a link", got.Now)
	}
	if len(got.Upcoming) != 1 || got.Upcoming[0].EventID != "evt-soon" || got.Upcoming[0].PlayerName != "Oscar Other" {
		t.Errorf("upcoming = %+v, want evt-soon following Oscar Other", got.Upcoming)
	}

	rec := fx.do(http.MethodGet, "/api/me/spectating/evt-soon", "", fan, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"p2"`) {
		t.Errorf("get one: status %d body %s", rec.Code, rec.Body.String())
	}

	// Revoking the link removes the event it saved.
	fx.do(http.MethodDelete, "/api/events/evt-live/follow-link", "", owner, nil)
	if got := list(); len(got.Now) != 0 || len(got.Upcoming) != 1 {
		t.Errorf("after revoking: now=%d upcoming=%d, want 0 and 1", len(got.Now), len(got.Upcoming))
	}

	// The spectator registering for the event moves it out of the list.
	if err := fx.users.SetBcpUserID(context.Background(), fanID, "u-other"); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got.Upcoming) != 0 {
		t.Errorf("after registering: upcoming = %+v, want empty", got.Upcoming)
	}

	fx.do(http.MethodDelete, "/api/me/spectating/evt-soon", "", fan, nil)
	if rec := fx.do(http.MethodGet, "/api/me/spectating/evt-soon", "", fan, nil); rec.Code != http.StatusNotFound {
		t.Errorf("after removing: status %d, want 404", rec.Code)
	}
}

func TestSearch_NormalizesValidatesAndCaches(t *testing.T) {
	var calls atomic.Int32
	var gotQuery atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotQuery.Store(r.URL.Query().Get("searchString"))
		_, _ = w.Write([]byte(`{"data": [{"id": "e1", "name": "Kawartha Open", "eventDate": "2026-11-07", "city": "Lindsay", "country": "Canada", "totalPlayers": 24}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	e := echo.New()
	passThrough := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	NewSearchHandler(bcp.NewClientWithBaseURL(server.URL)).Register(e, passThrough, passThrough)
	get := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/event-search?q="+q, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("ka"); rec.Code != http.StatusBadRequest {
		t.Errorf("2-character query: status %d, want 400", rec.Code)
	}
	if rec := get(strings.Repeat("a", 101)); rec.Code != http.StatusBadRequest {
		t.Errorf("101-character query: status %d, want 400", rec.Code)
	}

	rec := get("%20Kawartha%20%20Open")
	if rec.Code != http.StatusOK {
		t.Fatalf("search: status %d", rec.Code)
	}
	if q := gotQuery.Load(); q != "kawartha open" {
		t.Errorf("BCP got searchString %q, want lowercase and trimmed", q)
	}
	var results []bcp.EventSearchResult
	_ = json.Unmarshal(rec.Body.Bytes(), &results)
	if len(results) != 1 || results[0].Location != "Lindsay, Canada" || results[0].PlayerCount == nil || *results[0].PlayerCount != 24 {
		t.Errorf("results = %+v", results)
	}

	get("kawartha%20open")
	if n := calls.Load(); n != 1 {
		t.Errorf("BCP calls = %d, want 1 (the repeat query is cached)", n)
	}
}

func TestFollow_RefusesAnEventOverAWeekAgo(t *testing.T) {
	fx := newFollowFixture(t)
	owner, _ := fx.signIn(t, "owner", "u-owner")
	fan, _ := fx.signIn(t, "fan", "")

	if rec := fx.do(http.MethodPut, "/api/events/evt-old/follow-link", "", owner, nil); rec.Code != http.StatusConflict {
		t.Errorf("create link: status %d, want 409", rec.Code)
	}
	if code, saved := saveSpectating(t, fx, fan, `{"eventId": "evt-old", "playerId": "p1"}`); code != http.StatusConflict || saved {
		t.Errorf("follow a player: status %d saved=%t, want 409 and not saved", code, saved)
	}
}
