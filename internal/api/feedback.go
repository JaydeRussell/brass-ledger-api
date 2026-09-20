package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/feedback"
	"github.com/JaydeRussell/brass-ledger-api/internal/notify"
)

const (
	feedbackMessageMaxLen = 4000
	feedbackPageMaxLen    = 200
	feedbackEmailMaxLen   = 320
)

// feedbackNotifier is the notify.ResendNotifier surface FeedbackHandler
// needs — an interface, like bcpClient/userStore elsewhere in this
// package, so tests can fake it without a real Resend call.
type feedbackNotifier interface {
	NotifyFeedback(ctx context.Context, r notify.FeedbackReport) error
}

// feedbackStore is the feedback.Store surface FeedbackHandler (this
// file) and AdminHandler (admin.go) need — declared once here since both
// share it, same interface-at-the-point-of-use pattern as userStore.
type feedbackStore interface {
	Create(ctx context.Context, r feedback.Report) (feedback.Report, error)
	List(ctx context.Context) ([]feedback.Report, error)
	SetStatus(ctx context.Context, id int64, status string) error
	CountOpen(ctx context.Context) (int, error)
}

// FeedbackHandler is POST /api/feedback: a bug report or suggestion
// submitted through the frontend's floating feedback widget. Public —
// no session required, see Register — since a visitor can hit a bug
// before ever signing in (or without an account at all); if a valid
// session cookie IS present, the account's name/email is attached to
// the stored report and the alert for triage context, but its absence
// never blocks the submission. Admin-facing listing/resolving of what
// gets stored here lives on AdminHandler instead (admin.go) — this
// handler's own routes stay the one thing an anonymous caller can hit.
type FeedbackHandler struct {
	store    userStore
	reports  feedbackStore
	notifier feedbackNotifier
}

// NewFeedbackHandler builds a FeedbackHandler.
func NewFeedbackHandler(store userStore, reports feedbackStore, notifier feedbackNotifier) *FeedbackHandler {
	return &FeedbackHandler{store: store, reports: reports, notifier: notifier}
}

// Register wires this handler's route onto e. rateLimit is applied
// here rather than left to the caller to remember, since this is the
// one route in this whole API an anonymous, unauthenticated visitor can
// hit repeatedly with no session/approval gate to slow them down first.
func (h *FeedbackHandler) Register(e *echo.Echo, rateLimit echo.MiddlewareFunc) {
	e.POST("/api/feedback", h.Submit, rateLimit)
}

type feedbackRequest struct {
	Kind         string `json:"kind"`
	Message      string `json:"message"`
	Page         string `json:"page"`
	ContactEmail string `json:"contactEmail"`
}

// Submit is POST /api/feedback.
func (h *FeedbackHandler) Submit(c echo.Context) error {
	var req feedbackRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "message is required"})
	}
	if len(req.Message) > feedbackMessageMaxLen {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("message must be %d characters or fewer", feedbackMessageMaxLen)})
	}
	if req.Kind != "bug" && req.Kind != "suggestion" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": `kind must be "bug" or "suggestion"`})
	}
	if len(req.Page) > feedbackPageMaxLen {
		req.Page = req.Page[:feedbackPageMaxLen]
	}
	req.ContactEmail = strings.TrimSpace(req.ContactEmail)
	if len(req.ContactEmail) > feedbackEmailMaxLen {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "contact email is too long"})
	}

	submitterID, submitterName, submitterEmail := h.submitterInfo(c)

	// Persisted first — unlike the alert email below, this is the actual
	// record an admin will triage later (see AdminHandler's feedback
	// routes), so a write failure here is a real failure, not something
	// to swallow and pretend succeeded.
	if _, err := h.reports.Create(c.Request().Context(), feedback.Report{
		Kind:              req.Kind,
		Message:           req.Message,
		Page:              req.Page,
		ContactEmail:      req.ContactEmail,
		SubmittedByUserID: submitterID,
		SubmittedByName:   submitterName,
		SubmittedByEmail:  submitterEmail,
	}); err != nil {
		log.Printf("feedback: storing submission failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "couldn't save feedback"})
	}

	submittedBy := ""
	if submitterName != "" {
		submittedBy = fmt.Sprintf("%s <%s>", submitterName, submitterEmail)
	}
	report := notify.FeedbackReport{
		Kind:         req.Kind,
		Message:      req.Message,
		Page:         req.Page,
		ContactEmail: req.ContactEmail,
		SubmittedBy:  submittedBy,
	}
	// Fire-and-forget-ish: log a failure rather than fail the request —
	// same "the alert email is best-effort" contract as the new-signup
	// notification (see notify.ResendNotifier.NotifyNewSignup's doc
	// comment). The submission is already saved above; a Resend outage
	// shouldn't make the submitter feel it vanished.
	if err := h.notifier.NotifyFeedback(c.Request().Context(), report); err != nil {
		log.Printf("feedback: notify failed (submission still accepted): %v", err)
	}

	return c.NoContent(http.StatusAccepted)
}

// submitterInfo best-effort identifies the caller from a session cookie,
// if one's present and valid — never required (see Submit), just extra
// triage context attached for free when it's there. Returns zero values
// for an anonymous or invalid-session submitter.
func (h *FeedbackHandler) submitterInfo(c echo.Context) (userID int64, name, email string) {
	u, ok := resolveSession(c, h.store)
	if !ok {
		return 0, "", ""
	}
	return u.ID, u.Name, u.Email
}
