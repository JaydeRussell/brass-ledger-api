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

// SetStatus, SetRole, and ListUsers extend fakeUserStore (defined in
// auth_test.go) to satisfy the fuller userStore interface these routes
// need — same fake, same package, same pattern as me_test.go's
// SetBcpUserID.
func (f *fakeUserStore) SetStatus(_ context.Context, userID int64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub, u := range f.byGoogle {
		if u.ID == userID {
			u.Status = status
			f.byGoogle[sub] = u
			return nil
		}
	}
	return user.ErrSessionNotFound
}

func (f *fakeUserStore) SetRole(_ context.Context, userID int64, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub, u := range f.byGoogle {
		if u.ID == userID {
			u.Role = role
			f.byGoogle[sub] = u
			return nil
		}
	}
	return user.ErrSessionNotFound
}

// ListUsers returns every fake user sorted by id, so callers get a
// deterministic order despite Go's randomized map iteration — the real
// Store orders pending-first-then-newest (see its own doc comment), but
// nothing here needs that specific ordering to test approve/reject/
// role logic, just a stable one.
func (f *fakeUserStore) ListUsers(_ context.Context) ([]user.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	users := make([]user.User, 0, len(f.byGoogle))
	for _, u := range f.byGoogle {
		users = append(users, u)
	}
	for i := 1; i < len(users); i++ {
		for j := i; j > 0 && users[j-1].ID > users[j].ID; j-- {
			users[j-1], users[j] = users[j], users[j-1]
		}
	}
	return users, nil
}

// newSignedInUser creates a fake user (a distinct Google sub per call,
// via subSuffix) with the given role/status already set, and returns a
// session cookie for them plus their id — the admin-route tests need
// several different users (an admin, a pending target, etc.) in the
// same store, unlike me_test.go's single-fixed-user signedInSession.
func newSignedInUser(t *testing.T, store *fakeUserStore, subSuffix, role, status string) (*http.Cookie, int64) {
	t.Helper()
	ctx := context.Background()
	u, err := store.UpsertUserFromGoogle(ctx, "sub-"+subSuffix, subSuffix+"@example.com", "User "+subSuffix, "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	if err := store.SetRole(ctx, u.ID, role); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if err := store.SetStatus(ctx, u.ID, status); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	token, err := store.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: token}, u.ID
}

func newAdminTestEcho(store userStore) *echo.Echo {
	e := echo.New()
	NewAdminHandler(store).Register(e)
	return e
}

func TestAdminHandler_RequiresAdmin(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)

	if rec := doRequest(e, http.MethodGet, "/api/admin/users", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	pendingCookie, _ := newSignedInUser(t, store, "pending", user.RoleUser, user.StatusPending)
	if rec := doRequest(e, http.MethodGet, "/api/admin/users", []*http.Cookie{pendingCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("pending user: status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	approvedUserCookie, _ := newSignedInUser(t, store, "approved-user", user.RoleUser, user.StatusApproved)
	if rec := doRequest(e, http.MethodGet, "/api/admin/users", []*http.Cookie{approvedUserCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("approved non-admin: status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	adminCookie, _ := newSignedInUser(t, store, "admin", user.RoleAdmin, user.StatusApproved)
	if rec := doRequest(e, http.MethodGet, "/api/admin/users", []*http.Cookie{adminCookie}); rec.Code != http.StatusOK {
		t.Errorf("admin: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestAdminHandler_ListUsers(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)
	adminCookie, adminID := newSignedInUser(t, store, "admin", user.RoleAdmin, user.StatusApproved)
	_, pendingID := newSignedInUser(t, store, "pending", user.RoleUser, user.StatusPending)

	rec := doRequest(e, http.MethodGet, "/api/admin/users", []*http.Cookie{adminCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []adminUserResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d users, want 2: %+v", len(got), got)
	}

	byID := map[int64]adminUserResponse{}
	for _, u := range got {
		byID[u.ID] = u
	}
	if u, ok := byID[adminID]; !ok || u.Role != user.RoleAdmin || u.Status != user.StatusApproved {
		t.Errorf("admin entry = %+v, want role=%s status=%s", u, user.RoleAdmin, user.StatusApproved)
	}
	if u, ok := byID[pendingID]; !ok || u.Role != user.RoleUser || u.Status != user.StatusPending {
		t.Errorf("pending entry = %+v, want role=%s status=%s", u, user.RoleUser, user.StatusPending)
	}
}

func TestAdminHandler_ApproveAndReject(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)
	adminCookie, _ := newSignedInUser(t, store, "admin", user.RoleAdmin, user.StatusApproved)
	_, targetID := newSignedInUser(t, store, "target", user.RoleUser, user.StatusPending)

	approvePath := fmt.Sprintf("/api/admin/users/%d/approve", targetID)
	if rec := doRequest(e, http.MethodPost, approvePath, []*http.Cookie{adminCookie}); rec.Code != http.StatusNoContent {
		t.Fatalf("Approve: status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	target, err := store.GetUserBySession(context.Background(), mustSessionFor(t, store, targetID))
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if target.Status != user.StatusApproved {
		t.Errorf("status after approve = %q, want %q", target.Status, user.StatusApproved)
	}

	rejectPath := fmt.Sprintf("/api/admin/users/%d/reject", targetID)
	if rec := doRequest(e, http.MethodPost, rejectPath, []*http.Cookie{adminCookie}); rec.Code != http.StatusNoContent {
		t.Fatalf("Reject: status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	target, err = store.GetUserBySession(context.Background(), mustSessionFor(t, store, targetID))
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if target.Status != user.StatusRejected {
		t.Errorf("status after reject = %q, want %q", target.Status, user.StatusRejected)
	}

	// A non-admin can't approve/reject even themselves.
	nonAdminCookie, nonAdminID := newSignedInUser(t, store, "non-admin", user.RoleUser, user.StatusApproved)
	selfApprovePath := fmt.Sprintf("/api/admin/users/%d/approve", nonAdminID)
	if rec := doRequest(e, http.MethodPost, selfApprovePath, []*http.Cookie{nonAdminCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin self-approve: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAdminHandler_SetRole(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)
	adminCookie, adminID := newSignedInUser(t, store, "admin", user.RoleAdmin, user.StatusApproved)
	_, targetID := newSignedInUser(t, store, "target", user.RoleUser, user.StatusApproved)

	promotePath := fmt.Sprintf("/api/admin/users/%d/role", targetID)
	body := `{"role":"admin"}`
	if rec := doJSONRequest(e, http.MethodPost, promotePath, body, []*http.Cookie{adminCookie}); rec.Code != http.StatusNoContent {
		t.Fatalf("promote: status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	target, err := store.GetUserBySession(context.Background(), mustSessionFor(t, store, targetID))
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if target.Role != user.RoleAdmin {
		t.Errorf("role after promote = %q, want %q", target.Role, user.RoleAdmin)
	}

	// An invalid role value is rejected outright.
	invalidPath := fmt.Sprintf("/api/admin/users/%d/role", targetID)
	if rec := doJSONRequest(e, http.MethodPost, invalidPath, `{"role":"superadmin"}`, []*http.Cookie{adminCookie}); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid role: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// An admin can't demote their own account.
	selfDemotePath := fmt.Sprintf("/api/admin/users/%d/role", adminID)
	if rec := doJSONRequest(e, http.MethodPost, selfDemotePath, `{"role":"user"}`, []*http.Cookie{adminCookie}); rec.Code != http.StatusBadRequest {
		t.Errorf("self-demote: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// mustSessionFor issues a fresh session for an existing fake user id, so
// a test can read back that user's current state via
// GetUserBySession without needing ListUsers for a single lookup.
func mustSessionFor(t *testing.T, store *fakeUserStore, userID int64) string {
	t.Helper()
	token, err := store.CreateSession(context.Background(), userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return token
}

// doJSONRequest is doRequest (auth_test.go) plus a JSON request body —
// none of the other test files in this package need one (their POST
// routes take no body, or take it via doRequest's cookies-only shape),
// so it lives here rather than being added to the shared doRequest.
func doJSONRequest(e *echo.Echo, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}
