package api

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// AdminHandler is the access-control management surface migration 0007
// added: list every account, approve/reject a pending one, and
// promote/demote roles. Every route here requires requireAdmin.
type AdminHandler struct {
	store userStore
}

// NewAdminHandler builds an AdminHandler.
func NewAdminHandler(store userStore) *AdminHandler {
	return &AdminHandler{store: store}
}

// Register wires this handler's routes onto e.
func (h *AdminHandler) Register(e *echo.Echo) {
	e.GET("/api/admin/users", h.ListUsers)
	e.POST("/api/admin/users/:id/approve", h.Approve)
	e.POST("/api/admin/users/:id/reject", h.Reject)
	e.POST("/api/admin/users/:id/role", h.SetRole)
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

// ListUsers is GET /api/admin/users: every account, pending first (see
// Store.ListUsers's ordering).
func (h *AdminHandler) ListUsers(c echo.Context) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}
	users, err := h.store.ListUsers(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	resp := make([]adminUserResponse, len(users))
	for i, u := range users {
		resp[i] = toAdminUserResponse(u)
	}
	return c.JSON(http.StatusOK, resp)
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
