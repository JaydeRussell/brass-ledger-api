// Package notify sends admin-facing alert emails via Resend
// (https://resend.com) — "a brand-new account signed up and needs
// approval" (see internal/api/auth.go's Callback) and "a bug report or
// suggestion came in" (see internal/api/feedback.go's Submit).
// Deliberately narrow: a couple of admin alert emails, no generic
// multi-channel abstraction.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

const defaultBaseURL = "https://api.resend.com"

// ResendNotifier emails every address in `to` via Resend's API. Building
// one with an empty apiKey, from, or to is valid and simply disables it
// (see enabled) — the same "unset means quietly absent" contract as
// this service's Google OAuth config, so a deployment that hasn't set
// up Resend yet still starts and runs fine.
type ResendNotifier struct {
	http     *http.Client
	baseURL  string
	apiKey   string
	from     string
	to       []string
	adminURL string
}

// NewResendNotifier builds a ResendNotifier pointed at the real Resend
// API. adminURL is where the alert email sends admins to act on it
// (this service's frontend's /admin page).
func NewResendNotifier(apiKey, from string, to []string, adminURL string) *ResendNotifier {
	return newResendNotifier(defaultBaseURL, apiKey, from, to, adminURL)
}

// NewResendNotifierWithBaseURL is NewResendNotifier but pointed at an
// arbitrary base URL — for tests that stand up an httptest.Server
// instead of reaching the real Resend API.
func NewResendNotifierWithBaseURL(baseURL, apiKey, from string, to []string, adminURL string) *ResendNotifier {
	return newResendNotifier(baseURL, apiKey, from, to, adminURL)
}

func newResendNotifier(baseURL, apiKey, from string, to []string, adminURL string) *ResendNotifier {
	return &ResendNotifier{
		http:     &http.Client{Timeout: 15 * time.Second},
		baseURL:  baseURL,
		apiKey:   apiKey,
		from:     from,
		to:       to,
		adminURL: adminURL,
	}
}

func (n *ResendNotifier) enabled() bool {
	return n.apiKey != "" && n.from != "" && len(n.to) > 0
}

type resendEmailRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// NotifyNewSignup emails every admin address that u just signed up and
// is waiting on approval. A no-op (nil, no network call) if this
// notifier isn't configured — see enabled. Callers should log, not
// fail, on a non-nil error: a missed alert email is never a reason to
// break someone's sign-in.
func (n *ResendNotifier) NotifyNewSignup(ctx context.Context, u user.User) error {
	if !n.enabled() {
		return nil
	}
	return n.send(ctx,
		fmt.Sprintf("New Brass Ledger sign-up pending approval: %s", u.Name),
		fmt.Sprintf(
			"%s (%s) just signed up and is waiting on approval.\n\nReview it here: %s",
			u.Name, u.Email, n.adminURL,
		),
	)
}

// FeedbackReport is one bug report or suggestion submitted through the
// frontend's floating feedback widget — see internal/api/feedback.go,
// the one caller.
type FeedbackReport struct {
	// "bug" or "suggestion".
	Kind    string
	Message string
	// The frontend path the submitter was on when they opened the
	// widget — "" if it wasn't captured for some reason.
	Page string
	// An email the submitter volunteered for follow-up — "" if they
	// left it blank.
	ContactEmail string
	// "Name <email>" if a valid session cookie was present, "" for an
	// anonymous/signed-out submitter — see feedback.go's
	// submitterFromSession. Never required.
	SubmittedBy string
}

// NotifyFeedback emails every admin address that a bug report or
// suggestion came in. A no-op if this notifier isn't configured — same
// contract as NotifyNewSignup, including "callers should log, not
// fail, on error".
func (n *ResendNotifier) NotifyFeedback(ctx context.Context, r FeedbackReport) error {
	if !n.enabled() {
		return nil
	}

	from := r.SubmittedBy
	if from == "" {
		from = "anonymous"
	}
	contact := r.ContactEmail
	if contact == "" {
		contact = "(none given)"
	}
	page := r.Page
	if page == "" {
		page = "(not captured)"
	}

	return n.send(ctx,
		fmt.Sprintf("Brass Ledger %s report", r.Kind),
		fmt.Sprintf(
			"From: %s\nContact email: %s\nPage: %s\n\n%s",
			from, contact, page, r.Message,
		),
	)
}

// send is the actual Resend API call both NotifyNewSignup and
// NotifyFeedback share — everything above this point is just building
// the subject/body text for a given alert.
func (n *ResendNotifier) send(ctx context.Context, subject, text string) error {
	body, err := json.Marshal(resendEmailRequest{
		From:    n.from,
		To:      n.to,
		Subject: subject,
		Text:    text,
	})
	if err != nil {
		return fmt.Errorf("encoding Resend request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building Resend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.apiKey)

	res, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("requesting Resend: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("resend request failed: HTTP %d", res.StatusCode)
	}
	return nil
}
