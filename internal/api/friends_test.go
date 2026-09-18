package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// GetUserByBcpUserID's fake is defined once in me_test.go (added there
// by the public-dossiers PR, merged ahead of this one) — reused as-is
// rather than redefined here.

// SendFriendRequest mirrors the real store's partial-unique-index
// behavior (migration 0014): a pending or accepted request already
// existing in this exact direction blocks a duplicate; a declined one
// doesn't (see TestFriendRequest_Decline).
func (f *fakeUserStore) SendFriendRequest(_ context.Context, requesterID, recipientID int64) (user.FriendRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.friendRequests {
		blocking := fr.Status == user.FriendRequestPending || fr.Status == user.FriendRequestAccepted
		if blocking && fr.RequesterID == requesterID && fr.RecipientID == recipientID {
			return user.FriendRequest{}, user.ErrFriendRequestAlreadyExists
		}
	}
	f.nextFriendRequestID++
	fr := user.FriendRequest{
		ID:          f.nextFriendRequestID,
		RequesterID: requesterID,
		RecipientID: recipientID,
		Status:      user.FriendRequestPending,
		CreatedAt:   time.Now(),
	}
	f.friendRequests = append(f.friendRequests, fr)
	return fr, nil
}

func (f *fakeUserStore) nameForUserID(userID int64) string {
	for _, u := range f.byGoogle {
		if u.ID == userID {
			return u.Name
		}
	}
	return ""
}

func (f *fakeUserStore) ListIncomingFriendRequests(_ context.Context, userID int64) ([]user.FriendRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := []user.FriendRequest{}
	for _, fr := range f.friendRequests {
		if fr.RecipientID == userID && fr.Status == user.FriendRequestPending {
			fr.RequesterName = f.nameForUserID(fr.RequesterID)
			requests = append(requests, fr)
		}
	}
	return requests, nil
}

func (f *fakeUserStore) respondToFakeFriendRequest(requestID, recipientID int64, newStatus string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, fr := range f.friendRequests {
		if fr.ID == requestID && fr.RecipientID == recipientID && fr.Status == user.FriendRequestPending {
			f.friendRequests[i].Status = newStatus
			return nil
		}
	}
	return user.ErrFriendRequestNotFound
}

func (f *fakeUserStore) AcceptFriendRequest(_ context.Context, requestID, recipientID int64) error {
	return f.respondToFakeFriendRequest(requestID, recipientID, user.FriendRequestAccepted)
}

func (f *fakeUserStore) DeclineFriendRequest(_ context.Context, requestID, recipientID int64) error {
	return f.respondToFakeFriendRequest(requestID, recipientID, user.FriendRequestDeclined)
}

func (f *fakeUserStore) ListFriends(_ context.Context, userID int64) ([]user.Friend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	friends := []user.Friend{}
	for _, fr := range f.friendRequests {
		if fr.Status != user.FriendRequestAccepted {
			continue
		}
		var otherID int64
		switch userID {
		case fr.RequesterID:
			otherID = fr.RecipientID
		case fr.RecipientID:
			otherID = fr.RequesterID
		default:
			continue
		}
		for _, u := range f.byGoogle {
			if u.ID == otherID {
				friends = append(friends, user.Friend{UserID: u.ID, Name: u.Name, BcpUserID: u.BcpUserID})
			}
		}
	}
	return friends, nil
}

func (f *fakeUserStore) RemoveFriend(_ context.Context, userID, friendUserID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.friendRequests[:0]
	for _, fr := range f.friendRequests {
		isThisFriendship := fr.Status == user.FriendRequestAccepted &&
			((fr.RequesterID == userID && fr.RecipientID == friendUserID) ||
				(fr.RequesterID == friendUserID && fr.RecipientID == userID))
		if !isThisFriendship {
			kept = append(kept, fr)
		}
	}
	f.friendRequests = kept
	return nil
}

func (f *fakeUserStore) AreFriends(_ context.Context, userID, otherUserID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.friendRequests {
		if fr.Status != user.FriendRequestAccepted {
			continue
		}
		if (fr.RequesterID == userID && fr.RecipientID == otherUserID) ||
			(fr.RequesterID == otherUserID && fr.RecipientID == userID) {
			return true, nil
		}
	}
	return false, nil
}

func newFriendsTestEcho(store userStore, client *bcp.Client) *echo.Echo {
	e := echo.New()
	NewFriendsHandler(store, client).Register(e)
	return e
}

// secondSignedInSessionWithUserID signs in a *different* fake account
// than signedInSession's fixed "sub-1" — needed throughout this file to
// exercise two-sided flows (send/accept, friend-gated events). Distinct
// from sync_test.go's own secondSignedInSession (same "sub-2"/"Bea
// Brooks" fake account, added independently for that file's own
// two-account follow-count tests) since this one also needs the new
// account's user id back, which that one's callers never did.
func secondSignedInSessionWithUserID(t *testing.T, store *fakeUserStore) (*http.Cookie, int64) {
	t.Helper()
	u, _, err := store.UpsertUserFromGoogle(context.Background(), "sub-2", "b@example.com", "Bea Brooks", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	token, err := store.CreateSession(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: token}, u.ID
}

func TestFriends_RequireSignIn(t *testing.T) {
	e := newFriendsTestEcho(newFakeUserStore(), bcp.NewClient())
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/friends/requests"},
		{http.MethodGet, "/api/friends/requests"},
		{http.MethodPost, "/api/friends/requests/1/accept"},
		{http.MethodPost, "/api/friends/requests/1/decline"},
		{http.MethodGet, "/api/friends"},
		{http.MethodDelete, "/api/friends/1"},
		{http.MethodGet, "/api/friends/bcp-1/events"},
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

func TestSendFriendRequest_UnknownBcpUserID(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newFriendsTestEcho(store, bcp.NewClient())

	req := httptest.NewRequest(http.MethodPost, "/api/friends/requests", strings.NewReader(`{"recipientBcpUserId": "nobody"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestSendFriendRequest_RejectsSelf(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-me"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}
	e := newFriendsTestEcho(store, bcp.NewClient())

	req := httptest.NewRequest(http.MethodPost, "/api/friends/requests", strings.NewReader(`{"recipientBcpUserId": "bcp-me"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// fullFriendshipFixture wires up two signed-in fake accounts (A the
// requester, B the recipient), each linked to a bcpUserId, sharing one
// Echo instance — the setup every accept/decline/list/events test below
// needs.
type fullFriendshipFixture struct {
	store            *fakeUserStore
	e                *echo.Echo
	cookieA, cookieB *http.Cookie
	userA, userB     int64
}

func newFullFriendshipFixture(t *testing.T, client *bcp.Client) fullFriendshipFixture {
	t.Helper()
	store := newFakeUserStore()
	cookieA, userA := signedInSession(t, store)
	cookieB, userB := secondSignedInSessionWithUserID(t, store)
	ctx := context.Background()
	if err := store.SetBcpUserID(ctx, userA, "bcp-a"); err != nil {
		t.Fatalf("SetBcpUserID (A): %v", err)
	}
	if err := store.SetBcpUserID(ctx, userB, "bcp-b"); err != nil {
		t.Fatalf("SetBcpUserID (B): %v", err)
	}
	return fullFriendshipFixture{
		store: store, e: newFriendsTestEcho(store, client),
		cookieA: cookieA, cookieB: cookieB, userA: userA, userB: userB,
	}
}

func (fx fullFriendshipFixture) do(method, path string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	fx.e.ServeHTTP(rec, req)
	return rec
}

func TestFriendRequest_SendAcceptListUnfriend(t *testing.T) {
	fx := newFullFriendshipFixture(t, bcp.NewClient())

	// A sends B a request.
	sendRec := fx.do(http.MethodPost, "/api/friends/requests", fx.cookieA, `{"recipientBcpUserId": "bcp-b"}`)
	if sendRec.Code != http.StatusOK {
		t.Fatalf("send status = %d, want %d (body: %s)", sendRec.Code, http.StatusOK, sendRec.Body.String())
	}
	var sendBody map[string]any
	if err := json.Unmarshal(sendRec.Body.Bytes(), &sendBody); err != nil {
		t.Fatalf("couldn't parse send body: %v", err)
	}
	requestID := int64(sendBody["id"].(float64))

	// Sending the same request again is a conflict, not a duplicate.
	if dupRec := fx.do(http.MethodPost, "/api/friends/requests", fx.cookieA, `{"recipientBcpUserId": "bcp-b"}`); dupRec.Code != http.StatusConflict {
		t.Errorf("duplicate send status = %d, want %d", dupRec.Code, http.StatusConflict)
	}

	// B sees it in their incoming list.
	listRec := fx.do(http.MethodGet, "/api/friends/requests", fx.cookieB, "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list incoming status = %d, want %d", listRec.Code, http.StatusOK)
	}
	var incoming []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &incoming); err != nil {
		t.Fatalf("couldn't parse incoming body: %v", err)
	}
	if len(incoming) != 1 || incoming[0]["name"] != "Anna Adams" {
		t.Fatalf("incoming = %+v, want one request from Anna Adams", incoming)
	}

	// A isn't friends with B yet, and can't see B's events.
	if before, _ := fx.store.AreFriends(context.Background(), fx.userA, fx.userB); before {
		t.Fatal("AreFriends before accept = true, want false")
	}
	if eventsRec := fx.do(http.MethodGet, "/api/friends/bcp-b/events", fx.cookieA, ""); eventsRec.Code != http.StatusNotFound {
		t.Errorf("events before accept status = %d, want %d", eventsRec.Code, http.StatusNotFound)
	}

	// B accepts.
	acceptRec := fx.do(http.MethodPost, "/api/friends/requests/"+strconv.FormatInt(requestID, 10)+"/accept", fx.cookieB, "")
	if acceptRec.Code != http.StatusNoContent {
		t.Fatalf("accept status = %d, want %d (body: %s)", acceptRec.Code, http.StatusNoContent, acceptRec.Body.String())
	}
	// Accepting again (already resolved) 404s.
	if reAcceptRec := fx.do(http.MethodPost, "/api/friends/requests/"+strconv.FormatInt(requestID, 10)+"/accept", fx.cookieB, ""); reAcceptRec.Code != http.StatusNotFound {
		t.Errorf("re-accept status = %d, want %d", reAcceptRec.Code, http.StatusNotFound)
	}

	// Now they're friends, in both directions' ListFriends.
	friendsARec := fx.do(http.MethodGet, "/api/friends", fx.cookieA, "")
	var friendsA []map[string]any
	if err := json.Unmarshal(friendsARec.Body.Bytes(), &friendsA); err != nil {
		t.Fatalf("couldn't parse friendsA: %v", err)
	}
	if len(friendsA) != 1 || friendsA[0]["name"] != "Bea Brooks" {
		t.Fatalf("A's friends = %+v, want one entry for Bea Brooks", friendsA)
	}
	friendsBRec := fx.do(http.MethodGet, "/api/friends", fx.cookieB, "")
	var friendsB []map[string]any
	if err := json.Unmarshal(friendsBRec.Body.Bytes(), &friendsB); err != nil {
		t.Fatalf("couldn't parse friendsB: %v", err)
	}
	if len(friendsB) != 1 || friendsB[0]["name"] != "Anna Adams" {
		t.Fatalf("B's friends = %+v, want one entry for Anna Adams", friendsB)
	}

	// Unfriending (A removes B, using B's user id from friendsA above)
	friendUserID := int64(friendsA[0]["userId"].(float64))
	unfriendRec := fx.do(http.MethodDelete, "/api/friends/"+strconv.FormatInt(friendUserID, 10), fx.cookieA, "")
	if unfriendRec.Code != http.StatusNoContent {
		t.Fatalf("unfriend status = %d, want %d", unfriendRec.Code, http.StatusNoContent)
	}
	if after, _ := fx.store.AreFriends(context.Background(), fx.userA, fx.userB); after {
		t.Fatal("AreFriends after unfriend = true, want false")
	}
}

func TestFriendRequest_Decline(t *testing.T) {
	fx := newFullFriendshipFixture(t, bcp.NewClient())
	sendRec := fx.do(http.MethodPost, "/api/friends/requests", fx.cookieA, `{"recipientBcpUserId": "bcp-b"}`)
	var sendBody map[string]any
	if err := json.Unmarshal(sendRec.Body.Bytes(), &sendBody); err != nil {
		t.Fatalf("couldn't parse send body: %v", err)
	}
	requestID := int64(sendBody["id"].(float64))

	declineRec := fx.do(http.MethodPost, "/api/friends/requests/"+strconv.FormatInt(requestID, 10)+"/decline", fx.cookieB, "")
	if declineRec.Code != http.StatusNoContent {
		t.Fatalf("decline status = %d, want %d", declineRec.Code, http.StatusNoContent)
	}
	if areFriends, _ := fx.store.AreFriends(context.Background(), fx.userA, fx.userB); areFriends {
		t.Fatal("AreFriends after decline = true, want false")
	}

	// A declined request doesn't block a later real one between the
	// same two accounts.
	resendRec := fx.do(http.MethodPost, "/api/friends/requests", fx.cookieA, `{"recipientBcpUserId": "bcp-b"}`)
	if resendRec.Code != http.StatusOK {
		t.Errorf("resend after decline status = %d, want %d (body: %s)", resendRec.Code, http.StatusOK, resendRec.Body.String())
	}
}

func TestFriendRequest_AcceptWrongRecipientNotFound(t *testing.T) {
	fx := newFullFriendshipFixture(t, bcp.NewClient())
	sendRec := fx.do(http.MethodPost, "/api/friends/requests", fx.cookieA, `{"recipientBcpUserId": "bcp-b"}`)
	var sendBody map[string]any
	if err := json.Unmarshal(sendRec.Body.Bytes(), &sendBody); err != nil {
		t.Fatalf("couldn't parse send body: %v", err)
	}
	requestID := int64(sendBody["id"].(float64))

	// A (the requester, not the recipient) tries to accept their own
	// outgoing request — should 404, not succeed.
	rec := fx.do(http.MethodPost, "/api/friends/requests/"+strconv.FormatInt(requestID, 10)+"/accept", fx.cookieA, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestFriendEvents_AggregatesOnceFriends(t *testing.T) {
	server := stubBCPHistoryServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	fx := newFullFriendshipFixture(t, client)

	sendRec := fx.do(http.MethodPost, "/api/friends/requests", fx.cookieA, `{"recipientBcpUserId": "bcp-b"}`)
	var sendBody map[string]any
	if err := json.Unmarshal(sendRec.Body.Bytes(), &sendBody); err != nil {
		t.Fatalf("couldn't parse send body: %v", err)
	}
	requestID := int64(sendBody["id"].(float64))
	if rec := fx.do(http.MethodPost, "/api/friends/requests/"+strconv.FormatInt(requestID, 10)+"/accept", fx.cookieB, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("accept status = %d", rec.Code)
	}

	// A asks for B's events by B's bcpUserId — B's own linked profile,
	// same stub server stubBCPHistoryServer wires up for
	// TestMyEvents_*, just reached through the friends route instead of
	// /api/me/events.
	rec := fx.do(http.MethodGet, "/api/friends/bcp-b/events", fx.cookieA, "")
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
	if len(resp.Past)+len(resp.Present)+len(resp.Future) == 0 {
		t.Errorf("expected at least one classified event, got none: %+v", resp)
	}
}
