package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/feedback"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// AdminHandler is the admin-only management surface: migration 0007's
// account list/approve/reject/re-role routes, plus listing and
// resolving the bug reports/suggestions FeedbackHandler (feedback.go)
// stores. Every route here requires requireAdmin.
type AdminHandler struct {
	store    userStore
	feedback feedbackStore
}

// NewAdminHandler builds an AdminHandler.
func NewAdminHandler(store userStore, feedback feedbackStore) *AdminHandler {
	return &AdminHandler{store: store, feedback: feedback}
}

// Register wires this handler's routes onto e.
func (h *AdminHandler) Register(e *echo.Echo) {
	e.GET("/api/admin/users", h.ListUsers)
	e.POST("/api/admin/users/:id/approve", h.Approve)
	e.POST("/api/admin/users/:id/reject", h.Reject)
	e.POST("/api/admin/users/:id/role", h.SetRole)

	e.GET("/api/admin/feedback", h.ListFeedback)
	e.GET("/api/admin/feedback/open-count", h.FeedbackOpenCount)
	e.POST("/api/admin/feedback/:id/resolve", h.ResolveFeedback)
	e.POST("/api/admin/feedback/:id/reopen", h.ReopenFeedback)
}

// adminUserResponse is one row of GET /api/admin/users.
type adminUserResponse struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
	BcpUserID string `json:"bcpUserId"`
	Role      string `json:"role"`
	Status    string `json:"status"`
}

// adminUserCounts is GET /api/admin/users' "counts" field — every
// account by status, independent of that request's own status/q filter
// (see user.UserStatusCounts).
type adminUserCounts struct {
	All      int `json:"all"`
	Pending  int `json:"pending"`
	Approved int `json:"approved"`
	Rejected int `json:"rejected"`
}

// adminUsersResponse is the body of GET /api/admin/users: one page of
// accounts matching the request's status/q filter, that filter's total
// (for the caller to compute page count), and the unfiltered per-status
// counts every status tab's label needs.
type adminUsersResponse struct {
	Items  []adminUserResponse `json:"items"`
	Total  int                 `json:"total"`
	Counts adminUserCounts     `json:"counts"`
}

func toAdminUserResponse(u user.User) adminUserResponse {
	return adminUserResponse{
		ID:        u.ID,
		Email:     u.Email,
		Name:      u.Name,
		AvatarURL: u.AvatarURL,
		BcpUserID: u.BcpUserID,
		Role:      u.Role,
		Status:    u.Status,
	}
}

// ListUsers is GET /api/admin/users?status=&q=&page=&pageSize= — one
// page of accounts (pending first within "all"/no status filter, see
// Store.ListUsers's ordering), plus the unfiltered per-status counts
// every status tab's label needs. status defaults to "all"; page/
// pageSize default and clamp per Store.ListUsers.
func (h *AdminHandler) ListUsers(c echo.Context) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}

	status := c.QueryParam("status")
	if status == "" {
		status = "all"
	}
	if status != "all" && status != user.StatusPending && status != user.StatusApproved && status != user.StatusRejected {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": `status must be "all", "pending", "approved", or "rejected"`})
	}

	// Absent/malformed page or pageSize parse to 0, which Store.ListUsers
	// treats the same as "not given" (defaults/clamps from there) — no
	// separate validation needed for a value only this admin-only,
	// server-controlled UI ever sends.
	page, _ := strconv.Atoi(c.QueryParam("page"))
	pageSize, _ := strconv.Atoi(c.QueryParam("pageSize"))

	result, err := h.store.ListUsers(c.Request().Context(), user.ListUsersOptions{
		Status:   status,
		Search:   c.QueryParam("q"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	items := make([]adminUserResponse, len(result.Items))
	for i, u := range result.Items {
		items[i] = toAdminUserResponse(u)
	}
	return c.JSON(http.StatusOK, adminUsersResponse{
		Items: items,
		Total: result.Total,
		Counts: adminUserCounts{
			All:      result.Counts.All,
			Pending:  result.Counts.Pending,
			Approved: result.Counts.Approved,
			Rejected: result.Counts.Rejected,
		},
	})
}

// Approve is POST /api/admin/users/:id/approve.
func (h *AdminHandler) Approve(c echo.Context) error {
	return h.setStatus(c, user.StatusApproved)
}

// Reject is POST /api/admin/users/:id/reject.
func (h *AdminHandler) Reject(c echo.Context) error {
	return h.setStatus(c, user.StatusRejected)
}

func (h *AdminHandler) setStatus(c echo.Context, status string) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}
	targetID, err := parseUserID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid user id"})
	}
	if err := h.store.SetStatus(c.Request().Context(), targetID, status); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

type setRoleRequest struct {
	Role string `json:"role"`
}

// SetRole is POST /api/admin/users/:id/role — body {"role": "admin"|"user"}.
func (h *AdminHandler) SetRole(c echo.Context) error {
	admin, err := requireAdmin(c, h.store)
	if err != nil {
		return err
	}
	targetID, err := parseUserID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid user id"})
	}

	var req setRoleRequest
	if bindErr := c.Bind(&req); bindErr != nil || (req.Role != user.RoleAdmin && req.Role != user.RoleUser) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": `"role" must be "admin" or "user"`})
	}

	// An admin demoting their own only admin account would have no one
	// left to undo it (ADMIN_EMAILS-listed admins would self-heal on
	// their next sign-in, but a second admin promoted only via this API
	// and not listed there genuinely could lock themselves out).
	if targetID == admin.ID && req.Role != user.RoleAdmin {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "can't demote your own account"})
	}

	if err := h.store.SetRole(c.Request().Context(), targetID, req.Role); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func parseUserID(c echo.Context) (int64, error) {
	return strconv.ParseInt(c.Param("id"), 10, 64)
}

// adminFeedbackResponse is one row of GET /api/admin/feedback.
type adminFeedbackResponse struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Message      string `json:"message"`
	Page         string `json:"page"`
	ContactEmail string `json:"contactEmail"`
	// "Name <email>" if the submitter was signed in, "" for an anonymous
	// submission — same formatting as the alert email (see feedback.go).
	SubmittedBy string `json:"submittedBy"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt"` // RFC 3339
}

func toAdminFeedbackResponse(r feedback.Report) adminFeedbackResponse {
	submittedBy := ""
	if r.SubmittedByName != "" {
		submittedBy = r.SubmittedByName + " <" + r.SubmittedByEmail + ">"
	}
	return adminFeedbackResponse{
		ID:           r.ID,
		Kind:         r.Kind,
		Message:      r.Message,
		Page:         r.Page,
		ContactEmail: r.ContactEmail,
		SubmittedBy:  submittedBy,
		Status:       r.Status,
		CreatedAt:    r.CreatedAt.Format(time.RFC3339),
	}
}

// ListFeedback is GET /api/admin/feedback: every bug report/suggestion,
// open first (see feedback.Store.List's ordering).
func (h *AdminHandler) ListFeedback(c echo.Context) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}
	reports, err := h.feedback.List(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	resp := make([]adminFeedbackResponse, len(reports))
	for i, r := range reports {
		resp[i] = toAdminFeedbackResponse(r)
	}
	return c.JSON(http.StatusOK, resp)
}

// FeedbackOpenCount is GET /api/admin/feedback/open-count — a lightweight
// count the frontend's nav-drawer badge fetches once per app load,
// rather than the full list just to show a number.
func (h *AdminHandler) FeedbackOpenCount(c echo.Context) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}
	count, err := h.feedback.CountOpen(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]int{"count": count})
}

// ResolveFeedback is POST /api/admin/feedback/:id/resolve.
func (h *AdminHandler) ResolveFeedback(c echo.Context) error {
	return h.setFeedbackStatus(c, feedback.StatusResolved)
}

// ReopenFeedback is POST /api/admin/feedback/:id/reopen.
func (h *AdminHandler) ReopenFeedback(c echo.Context) error {
	return h.setFeedbackStatus(c, feedback.StatusOpen)
}

func (h *AdminHandler) setFeedbackStatus(c echo.Context, status string) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid feedback id"})
	}
	if err := h.feedback.SetStatus(c.Request().Context(), id, status); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// requireAdmin is requireApprovedUser plus Role == RoleAdmin — every
// route in this file needs it.
func requireAdmin(c echo.Context, store userStore) (user.User, error) {
	u, err := requireApprovedUser(c, store)
	if err != nil {
		return user.User{}, err
	}
	if u.Role != user.RoleAdmin {
		return user.User{}, c.JSON(http.StatusForbidden, map[string]string{"error": "admin only"})
	}
	return u, nil
}
