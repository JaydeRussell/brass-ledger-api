package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

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

// FeedbackHandler is POST /api/feedback: a bug report or suggestion
// submitted through the frontend's floating feedback widget. Public —
// no session required, see Register — since a visitor can hit a bug
// before ever signing in (or without an account at all); if a valid
// session cookie IS present, the account's name/email is attached to
// the alert for triage context, but its absence never blocks the
// submission.
type FeedbackHandler struct {
	store    userStore
	notifier feedbackNotifier
}

// NewFeedbackHandler builds a FeedbackHandler.
func NewFeedbackHandler(store userStore, notifier feedbackNotifier) *FeedbackHandler {
	return &FeedbackHandler{store: store, notifier: notifier}
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

	report := notify.FeedbackReport{
		Kind:         req.Kind,
		Message:      req.Message,
		Page:         req.Page,
		ContactEmail: req.ContactEmail,
		SubmittedBy:  h.submitterFromSession(c),
	}
	// Fire-and-forget-ish: log a failure rather than fail the request —
	// same "the alert email is best-effort" contract as the new-signup
	// notification (see notify.ResendNotifier.NotifyNewSignup's doc
	// comment). The submitter already did the work of writing the
	// report; a Resend outage shouldn't make them feel it vanished.
	if err := h.notifier.NotifyFeedback(c.Request().Context(), report); err != nil {
		log.Printf("feedback: notify failed (submission still accepted): %v", err)
	}

	return c.NoContent(http.StatusAccepted)
}

// submitterFromSession best-effort identifies the caller from a session
// cookie, if one's present and valid — never required (see Submit),
// just extra triage context attached for free when it's there.
func (h *FeedbackHandler) submitterFromSession(c echo.Context) string {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	u, err := h.store.GetUserBySession(c.Request().Context(), cookie.Value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s <%s>", u.Name, u.Email)
}
