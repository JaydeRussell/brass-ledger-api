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

	"github.com/JaydeRussell/brass-ledger-api/internal/feedback"
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

// ListUsers is a from-scratch (not database-backed) re-implementation of
// Store.ListUsers' filter/sort/paginate/count contract — sorted by id
// rather than the real store's newest-first (nothing here needs that
// specific tiebreak to test approve/reject/role/pagination logic, just
// a stable, deterministic one, given Go's randomized map iteration).
func (f *fakeUserStore) ListUsers(_ context.Context, opts user.ListUsersOptions) (user.ListUsersResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	all := make([]user.User, 0, len(f.byGoogle))
	for _, u := range f.byGoogle {
		all = append(all, u)
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j-1].ID > all[j].ID; j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}

	var counts user.UserStatusCounts
	for _, u := range all {
		counts.All++
		switch u.Status {
		case user.StatusPending:
			counts.Pending++
		case user.StatusApproved:
			counts.Approved++
		case user.StatusRejected:
			counts.Rejected++
		}
	}

	q := strings.ToLower(opts.Search)
	filtered := make([]user.User, 0, len(all))
	for _, u := range all {
		if opts.Status != "" && opts.Status != "all" && u.Status != opts.Status {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(u.Name), q) && !strings.Contains(strings.ToLower(u.Email), q) {
			continue
		}
		filtered = append(filtered, u)
	}

	statusRank := func(status string) int {
		switch status {
		case user.StatusPending:
			return 0
		case user.StatusApproved:
			return 1
		case user.StatusRejected:
			return 2
		default:
			return 3
		}
	}
	for i := 1; i < len(filtered); i++ {
		for j := i; j > 0 && statusRank(filtered[j-1].Status) > statusRank(filtered[j].Status); j-- {
			filtered[j-1], filtered[j] = filtered[j], filtered[j-1]
		}
	}

	page := opts.Page
	if page < 1 {
		page = 1
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = user.DefaultUsersPageSize
	}
	if pageSize > user.MaxUsersPageSize {
		pageSize = user.MaxUsersPageSize
	}
	start := min((page-1)*pageSize, len(filtered))
	end := min(start+pageSize, len(filtered))

	return user.ListUsersResult{Items: filtered[start:end], Total: len(filtered), Counts: counts}, nil
}

// newSignedInUser creates a fake user (a distinct Google sub per call,
// via subSuffix) with the given role/status already set, and returns a
// session cookie for them plus their id — the admin-route tests need
// several different users (an admin, a pending target, etc.) in the
// same store, unlike me_test.go's single-fixed-user signedInSession.
func newSignedInUser(t *testing.T, store *fakeUserStore, subSuffix, role, status string) (*http.Cookie, int64) {
	t.Helper()
	ctx := context.Background()
	u, _, err := store.UpsertUserFromGoogle(ctx, "sub-"+subSuffix, subSuffix+"@example.com", "User "+subSuffix, "")
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
	return newAdminTestEchoWithFeedback(store, newFakeFeedbackStore())
}

func newAdminTestEchoWithFeedback(store userStore, reports feedbackStore) *echo.Echo {
	e := echo.New()
	NewAdminHandler(store, reports).Register(e)
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
	var got adminUsersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Items) != 2 || got.Total != 2 {
		t.Fatalf("got %d items (total %d), want 2: %+v", len(got.Items), got.Total, got)
	}
	if got.Counts.All != 2 || got.Counts.Pending != 1 || got.Counts.Approved != 1 {
		t.Fatalf("counts = %+v, want all=2 pending=1 approved=1", got.Counts)
	}

	byID := map[int64]adminUserResponse{}
	for _, u := range got.Items {
		byID[u.ID] = u
	}
	if u, ok := byID[adminID]; !ok || u.Role != user.RoleAdmin || u.Status != user.StatusApproved {
		t.Errorf("admin entry = %+v, want role=%s status=%s", u, user.RoleAdmin, user.StatusApproved)
	}
	if u, ok := byID[pendingID]; !ok || u.Role != user.RoleUser || u.Status != user.StatusPending {
		t.Errorf("pending entry = %+v, want role=%s status=%s", u, user.RoleUser, user.StatusPending)
	}
}

func TestAdminHandler_ListUsers_StatusFilterAndValidation(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)
	adminCookie, _ := newSignedInUser(t, store, "admin", user.RoleAdmin, user.StatusApproved)
	newSignedInUser(t, store, "pending-1", user.RoleUser, user.StatusPending)
	newSignedInUser(t, store, "pending-2", user.RoleUser, user.StatusPending)

	rec := doRequest(e, http.MethodGet, "/api/admin/users?status=pending", []*http.Cookie{adminCookie})
	var got adminUsersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Items) != 2 || got.Total != 2 {
		t.Fatalf("status=pending: got %d items (total %d), want 2", len(got.Items), got.Total)
	}
	// The unfiltered counts (3 accounts total: 1 admin, 2 pending) don't
	// change just because this request filtered to one status.
	if got.Counts.All != 3 {
		t.Errorf("counts.All = %d, want 3 (unaffected by the status filter)", got.Counts.All)
	}

	if rec := doRequest(e, http.MethodGet, "/api/admin/users?status=bogus", []*http.Cookie{adminCookie}); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid status: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestAdminHandler_ListUsers_SearchAndPagination(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)
	adminCookie, _ := newSignedInUser(t, store, "admin", user.RoleAdmin, user.StatusApproved)
	for i := 0; i < 5; i++ {
		newSignedInUser(t, store, fmt.Sprintf("bulk-%d", i), user.RoleUser, user.StatusPending)
	}

	// pageSize=2 over 6 accounts (5 bulk + 1 admin) → 3 pages, 2 items each.
	rec := doRequest(e, http.MethodGet, "/api/admin/users?pageSize=2&page=1", []*http.Cookie{adminCookie})
	var page1 adminUsersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(page1.Items) != 2 || page1.Total != 6 {
		t.Fatalf("page 1: got %d items (total %d), want 2 items, total 6", len(page1.Items), page1.Total)
	}

	rec = doRequest(e, http.MethodGet, "/api/admin/users?pageSize=2&page=2", []*http.Cookie{adminCookie})
	var page2 adminUsersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(page2.Items) != 2 || page2.Items[0].ID == page1.Items[0].ID {
		t.Fatalf("page 2 should be 2 different items than page 1: page1=%+v page2=%+v", page1.Items, page2.Items)
	}

	// A search that matches only the admin account (its Google sub is
	// "sub-admin" — see newSignedInUser — giving it name/email "User admin"/
	// "admin@example.com", not matched by the "bulk-" accounts' emails).
	rec = doRequest(e, http.MethodGet, "/api/admin/users?q=admin", []*http.Cookie{adminCookie})
	var searched adminUsersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &searched); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(searched.Items) != 1 || searched.Total != 1 {
		t.Fatalf("q=admin: got %d items (total %d), want 1", len(searched.Items), searched.Total)
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

func TestAdminHandler_FeedbackRequiresAdmin(t *testing.T) {
	store := newFakeUserStore()
	e := newAdminTestEcho(store)

	if rec := doRequest(e, http.MethodGet, "/api/admin/feedback", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	approvedUserCookie, _ := newSignedInUser(t, store, "approved-user-fb", user.RoleUser, user.StatusApproved)
	if rec := doRequest(e, http.MethodGet, "/api/admin/feedback", []*http.Cookie{approvedUserCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("approved non-admin: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAdminHandler_ListFeedbackAndResolve(t *testing.T) {
	store := newFakeUserStore()
	reports := newFakeFeedbackStore()
	e := newAdminTestEchoWithFeedback(store, reports)
	adminCookie, _ := newSignedInUser(t, store, "admin-fb", user.RoleAdmin, user.StatusApproved)

	created, err := reports.Create(context.Background(), feedback.Report{Kind: "bug", Message: "it broke"})
	if err != nil {
		t.Fatalf("seeding feedback: %v", err)
	}

	rec := doRequest(e, http.MethodGet, "/api/admin/feedback", []*http.Cookie{adminCookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []adminFeedbackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 1 || got[0].Status != feedback.StatusOpen || got[0].Message != "it broke" {
		t.Fatalf("unexpected list: %+v", got)
	}

	countRec := doRequest(e, http.MethodGet, "/api/admin/feedback/open-count", []*http.Cookie{adminCookie})
	var countResp map[string]int
	if err := json.Unmarshal(countRec.Body.Bytes(), &countResp); err != nil || countResp["count"] != 1 {
		t.Fatalf("open-count = %v (err %v), want {\"count\":1}", countResp, err)
	}

	resolvePath := fmt.Sprintf("/api/admin/feedback/%d/resolve", created.ID)
	if rec := doRequest(e, http.MethodPost, resolvePath, []*http.Cookie{adminCookie}); rec.Code != http.StatusNoContent {
		t.Fatalf("resolve: status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	afterResolve, _ := reports.List(context.Background())
	if afterResolve[0].Status != feedback.StatusResolved {
		t.Errorf("status after resolve = %q, want %q", afterResolve[0].Status, feedback.StatusResolved)
	}

	reopenPath := fmt.Sprintf("/api/admin/feedback/%d/reopen", created.ID)
	if rec := doRequest(e, http.MethodPost, reopenPath, []*http.Cookie{adminCookie}); rec.Code != http.StatusNoContent {
		t.Fatalf("reopen: status = %d, want %d, body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	afterReopen, _ := reports.List(context.Background())
	if afterReopen[0].Status != feedback.StatusOpen {
		t.Errorf("status after reopen = %q, want %q", afterReopen[0].Status, feedback.StatusOpen)
	}
}
