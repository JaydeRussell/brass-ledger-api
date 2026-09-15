package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/notify"
)

// fakeFeedbackNotifier is an in-memory stand-in for
// *notify.ResendNotifier, so FeedbackHandler's own logic (validation,
// status codes, session lookup) can be tested without a real Resend
// call — same reasoning as fakeUserStore for *user.Store.
type fakeFeedbackNotifier struct {
	got     []notify.FeedbackReport
	failErr error
}

func (f *fakeFeedbackNotifier) NotifyFeedback(_ context.Context, r notify.FeedbackReport) error {
	f.got = append(f.got, r)
	return f.failErr
}

func newFeedbackTestEcho(store userStore, notifier feedbackNotifier) *echo.Echo {
	e := echo.New()
	NewFeedbackHandler(store, notifier).Register(e, noopMiddleware)
	return e
}

func noopMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return next
}

func postFeedback(e *echo.Echo, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestFeedbackHandler_Submit(t *testing.T) {
	notifier := &fakeFeedbackNotifier{}
	e := newFeedbackTestEcho(newFakeUserStore(), notifier)

	rec := postFeedback(e, `{"kind":"bug","message":"Overview shows a blank page.","page":"/?event=abc123"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if len(notifier.got) != 1 {
		t.Fatalf("expected exactly one NotifyFeedback call, got %d", len(notifier.got))
	}
	got := notifier.got[0]
	if got.Kind != "bug" || got.Message != "Overview shows a blank page." || got.Page != "/?event=abc123" {
		t.Errorf("unexpected report: %+v", got)
	}
	if got.SubmittedBy != "" {
		t.Errorf("SubmittedBy = %q, want empty for an anonymous submission", got.SubmittedBy)
	}
}

func TestFeedbackHandler_Submit_AttachesSignedInSubmitter(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := newSignedInUser(t, store, "feedback-submitter", "user", "approved")
	notifier := &fakeFeedbackNotifier{}
	e := newFeedbackTestEcho(store, notifier)

	rec := postFeedback(e, `{"kind":"suggestion","message":"Add dark mode."}`, cookie)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	want := "User feedback-submitter <feedback-submitter@example.com>"
	if notifier.got[0].SubmittedBy != want {
		t.Errorf("SubmittedBy = %q, want %q", notifier.got[0].SubmittedBy, want)
	}
}

func TestFeedbackHandler_Submit_InvalidSessionCookieStillSucceeds(t *testing.T) {
	notifier := &fakeFeedbackNotifier{}
	e := newFeedbackTestEcho(newFakeUserStore(), notifier)

	rec := postFeedback(e, `{"kind":"bug","message":"test"}`, &http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if notifier.got[0].SubmittedBy != "" {
		t.Errorf("SubmittedBy = %q, want empty for a bad session cookie", notifier.got[0].SubmittedBy)
	}
}

func TestFeedbackHandler_Submit_Validation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty message", `{"kind":"bug","message":""}`},
		{"whitespace-only message", `{"kind":"bug","message":"   "}`},
		{"missing kind", `{"message":"test"}`},
		{"invalid kind", `{"kind":"complaint","message":"test"}`},
		{"message too long", `{"kind":"bug","message":"` + strings.Repeat("a", feedbackMessageMaxLen+1) + `"}`},
		{"contact email too long", `{"kind":"bug","message":"test","contactEmail":"` + strings.Repeat("a", feedbackEmailMaxLen+1) + `"}`},
		{"malformed json", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier := &fakeFeedbackNotifier{}
			e := newFeedbackTestEcho(newFakeUserStore(), notifier)

			rec := postFeedback(e, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if len(notifier.got) != 0 {
				t.Errorf("expected no NotifyFeedback call for invalid input, got %d", len(notifier.got))
			}
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["error"] == "" {
				t.Errorf("expected a JSON {error} response, got %s", rec.Body.String())
			}
		})
	}
}

func TestFeedbackHandler_Submit_LongPageIsTruncatedNotRejected(t *testing.T) {
	notifier := &fakeFeedbackNotifier{}
	e := newFeedbackTestEcho(newFakeUserStore(), notifier)

	longPage := "/" + strings.Repeat("a", feedbackPageMaxLen+50)
	rec := postFeedback(e, `{"kind":"bug","message":"test","page":"`+longPage+`"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if len(notifier.got[0].Page) != feedbackPageMaxLen {
		t.Errorf("page length = %d, want %d", len(notifier.got[0].Page), feedbackPageMaxLen)
	}
}

func TestFeedbackHandler_Submit_NotifyErrorStillAccepted(t *testing.T) {
	notifier := &fakeFeedbackNotifier{failErr: context.DeadlineExceeded}
	e := newFeedbackTestEcho(newFakeUserStore(), notifier)

	rec := postFeedback(e, `{"kind":"bug","message":"test"}`)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d — a failed alert email should never fail the submission", rec.Code, http.StatusAccepted)
	}
}
