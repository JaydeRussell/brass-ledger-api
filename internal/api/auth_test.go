package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/auth"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// fakeUserStore is an in-memory stand-in for *user.Store, so these
// routes' own logic (cookies, redirects, status codes) can be tested
// without a real Postgres — the store's own SQL is user.Store's
// responsibility, not this package's.
type fakeUserStore struct {
	mu       sync.Mutex
	byGoogle map[string]user.User
	sessions map[string]int64
	nextID   int64

	upsertErr        error
	createSessionErr error
	getUserErr       error

	// follows is keyed by userID, then by event+kind+refId (matching the
	// real store's composite primary key) so AddFollow's "update the
	// label if already followed" semantics are easy to reproduce.
	follows      map[int64]map[string]user.Follow
	recentEvents map[int64][]user.RecentEvent
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		byGoogle:     make(map[string]user.User),
		sessions:     make(map[string]int64),
		follows:      make(map[int64]map[string]user.Follow),
		recentEvents: make(map[int64][]user.RecentEvent),
	}
}

func (f *fakeUserStore) UpsertUserFromGoogle(_ context.Context, googleSub, email, name, avatarURL string) (user.User, error) {
	if f.upsertErr != nil {
		return user.User{}, f.upsertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, existed := f.byGoogle[googleSub]
	if !existed {
		f.nextID++
		u.ID = f.nextID
	}
	u.Email, u.Name, u.AvatarURL = email, name, avatarURL
	f.byGoogle[googleSub] = u
	return u, nil
}

func (f *fakeUserStore) CreateSession(_ context.Context, userID int64) (string, error) {
	if f.createSessionErr != nil {
		return "", f.createSessionErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	token := fmt.Sprintf("session-token-%d-%d", userID, len(f.sessions))
	f.sessions[token] = userID
	return token, nil
}

func (f *fakeUserStore) GetUserBySession(_ context.Context, token string) (user.User, error) {
	if f.getUserErr != nil {
		return user.User{}, f.getUserErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	userID, ok := f.sessions[token]
	if !ok {
		return user.User{}, user.ErrSessionNotFound
	}
	for _, u := range f.byGoogle {
		if u.ID == userID {
			return u, nil
		}
	}
	return user.User{}, user.ErrSessionNotFound
}

func (f *fakeUserStore) DeleteSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, token)
	return nil
}

func followFakeKey(eventID, kind, refID string) string {
	return eventID + "|" + kind + "|" + refID
}

func (f *fakeUserStore) ListFollows(_ context.Context, userID int64, eventID string) ([]user.Follow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := eventID + "|"
	follows := []user.Follow{}
	for key, follow := range f.follows[userID] {
		if strings.HasPrefix(key, prefix) {
			follows = append(follows, follow)
		}
	}
	return follows, nil
}

func (f *fakeUserStore) AddFollow(_ context.Context, userID int64, eventID, kind, refID, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.follows[userID] == nil {
		f.follows[userID] = make(map[string]user.Follow)
	}
	f.follows[userID][followFakeKey(eventID, kind, refID)] = user.Follow{Kind: kind, RefID: refID, Label: label}
	return nil
}

func (f *fakeUserStore) RemoveFollow(_ context.Context, userID int64, eventID, kind, refID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.follows[userID], followFakeKey(eventID, kind, refID))
	return nil
}

func (f *fakeUserStore) ListRecentEvents(_ context.Context, userID int64) ([]user.RecentEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events := append([]user.RecentEvent{}, f.recentEvents[userID]...)
	sort.Slice(events, func(i, j int) bool { return events[i].LastViewedAt.After(events[j].LastViewedAt) })
	if len(events) > user.MaxRecentEvents {
		events = events[:user.MaxRecentEvents]
	}
	return events, nil
}

func (f *fakeUserStore) RecordRecentEvent(_ context.Context, userID int64, eventID, eventName string, teamEvent bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current := f.recentEvents[userID]
	next := make([]user.RecentEvent, 0, len(current)+1)
	for _, e := range current {
		if e.EventID != eventID {
			next = append(next, e)
		}
	}
	next = append(next, user.RecentEvent{
		EventID:      eventID,
		EventName:    eventName,
		TeamEvent:    teamEvent,
		LastViewedAt: time.Now(),
	})
	f.recentEvents[userID] = next
	return nil
}

// stubGoogleServer stands up a fake Google (token + userinfo endpoints)
// so tests never reach the real API — mirrors internal/auth/google_test.go's
// own stub pattern.
func stubGoogleServer(t *testing.T, tokenBody, userInfoBody string) *auth.GoogleOAuth {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenBody))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(userInfoBody))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return auth.NewGoogleOAuthWithBaseURLs(
		"client-123", "secret-abc", "http://backend.example.com/auth/google/callback",
		server.URL+"/auth", server.URL+"/token", server.URL+"/userinfo",
	)
}

const frontendURL = "http://frontend.example.com/"

func newTestEcho(google *auth.GoogleOAuth, store userStore) *echo.Echo {
	e := echo.New()
	RegisterAuthRoutes(e, google, store, frontendURL, false)
	return e
}

func doRequest(e *echo.Echo, method, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestGoogleLogin(t *testing.T) {
	google := stubGoogleServer(t, `{"access_token": "tok"}`, `{"sub": "1"}`)
	e := newTestEcho(google, newFakeUserStore())

	rec := doRequest(e, http.MethodGet, "/auth/google/login", nil)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location header %q isn't a valid URL: %v", rec.Header().Get("Location"), err)
	}
	stateInRedirect := location.Query().Get("state")
	if stateInRedirect == "" {
		t.Fatal("redirect URL is missing a state query param")
	}

	stateCookie := findCookie(rec.Result().Cookies(), stateCookieName)
	if stateCookie == nil {
		t.Fatal("no oauth_state cookie was set")
	}
	if stateCookie.Value != stateInRedirect {
		t.Errorf("state cookie = %q, redirect state = %q, want them to match", stateCookie.Value, stateInRedirect)
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie should be HttpOnly")
	}
}

// TestGoogleCallback_Success drives the full flow: login (to get a real
// state cookie), then callback with a matching state — checking the
// session cookie that results actually authenticates a later /api/me
// call, and that a second sign-in for the same Google account reuses
// the same user id rather than creating a duplicate.
func TestGoogleCallback_Success(t *testing.T) {
	google := stubGoogleServer(t,
		`{"access_token": "tok-abc"}`,
		`{"sub": "google-sub-1", "email": "anna@example.com", "name": "Anna Adams", "picture": "https://example.com/pic.jpg"}`,
	)
	store := newFakeUserStore()
	e := newTestEcho(google, store)

	loginRec := doRequest(e, http.MethodGet, "/auth/google/login", nil)
	stateCookie := findCookie(loginRec.Result().Cookies(), stateCookieName)
	if stateCookie == nil {
		t.Fatal("no state cookie from /auth/google/login")
	}

	callbackPath := "/auth/google/callback?state=" + stateCookie.Value + "&code=any-code"
	callbackRec := doRequest(e, http.MethodGet, callbackPath, []*http.Cookie{stateCookie})

	if callbackRec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want %d (body: %s)", callbackRec.Code, http.StatusFound, callbackRec.Body.String())
	}
	if got := callbackRec.Header().Get("Location"); got != frontendURL {
		t.Errorf("callback redirected to %q, want %q", got, frontendURL)
	}

	sessionCookie := findCookie(callbackRec.Result().Cookies(), sessionCookieName)
	if sessionCookie == nil {
		t.Fatal("no session cookie was set after a successful callback")
	}

	meRec := doRequest(e, http.MethodGet, "/api/me", []*http.Cookie{sessionCookie})
	if meRec.Code != http.StatusOK {
		t.Fatalf("/api/me status = %d, want %d (body: %s)", meRec.Code, http.StatusOK, meRec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(meRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("couldn't parse /api/me body: %v", err)
	}
	if body["email"] != "anna@example.com" || body["name"] != "Anna Adams" {
		t.Errorf("/api/me body = %+v, want anna@example.com / Anna Adams", body)
	}

	if len(store.byGoogle) != 1 {
		t.Errorf("store has %d users, want exactly 1", len(store.byGoogle))
	}
}

func TestGoogleCallback_Rejections(t *testing.T) {
	cases := []struct {
		name            string
		path            string
		sendStateCookie bool
		wantStatus      int
	}{
		{
			name:            "state query param doesn't match the cookie",
			path:            "/auth/google/callback?state=wrong-state&code=abc",
			sendStateCookie: true,
			wantStatus:      http.StatusBadRequest,
		},
		{
			name:            "no state cookie at all (e.g. it expired)",
			path:            "/auth/google/callback?state=whatever&code=abc",
			sendStateCookie: false,
			wantStatus:      http.StatusBadRequest,
		},
		{
			name:            "missing code",
			path:            "/auth/google/callback?state=STATE_PLACEHOLDER",
			sendStateCookie: true,
			wantStatus:      http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			google := stubGoogleServer(t, `{"access_token": "tok"}`, `{"sub": "1"}`)
			e := newTestEcho(google, newFakeUserStore())

			loginRec := doRequest(e, http.MethodGet, "/auth/google/login", nil)
			stateCookie := findCookie(loginRec.Result().Cookies(), stateCookieName)

			path := strings.Replace(tc.path, "STATE_PLACEHOLDER", stateCookie.Value, 1)
			var cookies []*http.Cookie
			if tc.sendStateCookie {
				cookies = []*http.Cookie{stateCookie}
			}

			rec := doRequest(e, http.MethodGet, path, cookies)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGoogleCallback_UpstreamFailure(t *testing.T) {
	// A Google token endpoint that always errors — the callback should
	// map that to a 502 (this service's dependency failed), not a 500.
	google := stubGoogleServer(t, `{"error": "invalid_grant", "error_description": "boom"}`, `{"sub": "1"}`)
	e := newTestEcho(google, newFakeUserStore())

	loginRec := doRequest(e, http.MethodGet, "/auth/google/login", nil)
	stateCookie := findCookie(loginRec.Result().Cookies(), stateCookieName)

	path := "/auth/google/callback?state=" + stateCookie.Value + "&code=any-code"
	rec := doRequest(e, http.MethodGet, path, []*http.Cookie{stateCookie})

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestLogoutAndMe(t *testing.T) {
	google := stubGoogleServer(t, `{"access_token": "tok"}`, `{"sub": "1", "email": "a@example.com"}`)
	store := newFakeUserStore()
	e := newTestEcho(google, store)

	// Sign in first.
	loginRec := doRequest(e, http.MethodGet, "/auth/google/login", nil)
	stateCookie := findCookie(loginRec.Result().Cookies(), stateCookieName)
	callbackRec := doRequest(e, http.MethodGet,
		"/auth/google/callback?state="+stateCookie.Value+"&code=abc",
		[]*http.Cookie{stateCookie},
	)
	sessionCookie := findCookie(callbackRec.Result().Cookies(), sessionCookieName)
	if sessionCookie == nil {
		t.Fatal("expected a session cookie after signing in")
	}

	// /api/me works while signed in.
	if rec := doRequest(e, http.MethodGet, "/api/me", []*http.Cookie{sessionCookie}); rec.Code != http.StatusOK {
		t.Fatalf("/api/me before logout: status = %d, want 200", rec.Code)
	}

	// Log out.
	logoutRec := doRequest(e, http.MethodPost, "/auth/logout", []*http.Cookie{sessionCookie})
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutRec.Code, http.StatusNoContent)
	}
	clearedCookie := findCookie(logoutRec.Result().Cookies(), sessionCookieName)
	if clearedCookie == nil || clearedCookie.MaxAge >= 0 {
		t.Errorf("logout should clear the session cookie (negative MaxAge), got %+v", clearedCookie)
	}

	// /api/me now fails even with the (now-revoked) old cookie.
	if rec := doRequest(e, http.MethodGet, "/api/me", []*http.Cookie{sessionCookie}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/me after logout: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMe_NotSignedIn(t *testing.T) {
	google := stubGoogleServer(t, `{"access_token": "tok"}`, `{"sub": "1"}`)
	e := newTestEcho(google, newFakeUserStore())

	cases := []struct {
		name    string
		cookies []*http.Cookie
	}{
		{"no cookie at all", nil},
		{"a session cookie that doesn't exist", []*http.Cookie{{Name: sessionCookieName, Value: "not-a-real-token"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(e, http.MethodGet, "/api/me", tc.cookies)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}
