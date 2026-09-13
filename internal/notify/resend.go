// Package notify sends admin-facing alert emails via Resend
// (https://resend.com) — currently just "a brand-new account signed up
// and needs approval" (see internal/api/auth.go's Callback, the one
// caller). Deliberately narrow: one notifier, one email, no generic
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

	body, err := json.Marshal(resendEmailRequest{
		From:    n.from,
		To:      n.to,
		Subject: fmt.Sprintf("New Brass Ledger sign-up pending approval: %s", u.Name),
		Text: fmt.Sprintf(
			"%s (%s) just signed up and is waiting on approval.\n\nReview it here: %s",
			u.Name, u.Email, n.adminURL,
		),
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
