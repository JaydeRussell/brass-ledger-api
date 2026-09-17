package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/feedback"
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

// fakeFeedbackStore is an in-memory stand-in for *feedback.Store, shared
// by feedback_test.go (Submit) and admin_test.go (List/SetStatus/
// CountOpen) — same reasoning as fakeUserStore for *user.Store.
type fakeFeedbackStore struct {
	mu      sync.Mutex
	reports []feedback.Report
	nextID  int64
	failErr error
}

func newFakeFeedbackStore() *fakeFeedbackStore {
	return &fakeFeedbackStore{}
}

func (f *fakeFeedbackStore) Create(_ context.Context, r feedback.Report) (feedback.Report, error) {
	if f.failErr != nil {
		return feedback.Report{}, f.failErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	r.ID = f.nextID
	r.Status = feedback.StatusOpen
	r.CreatedAt = time.Now()
	f.reports = append(f.reports, r)
	return r, nil
}

func (f *fakeFeedbackStore) List(_ context.Context) ([]feedback.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]feedback.Report, len(f.reports))
	copy(out, f.reports)
	return out, nil
}

func (f *fakeFeedbackStore) SetStatus(_ context.Context, id int64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.reports {
		if r.ID == id {
			f.reports[i].Status = status
			return nil
		}
	}
	return errNotFound
}

var errNotFound = errors.New("feedback not found")

func (f *fakeFeedbackStore) CountOpen(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reports {
		if r.Status == feedback.StatusOpen {
			n++
		}
	}
	return n, nil
}

func newFeedbackTestEcho(store userStore, reports feedbackStore, notifier feedbackNotifier) *echo.Echo {
	e := echo.New()
	NewFeedbackHandler(store, reports, notifier).Register(e, noopMiddleware)
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
	reports := newFakeFeedbackStore()
	e := newFeedbackTestEcho(newFakeUserStore(), reports, notifier)

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

	stored, _ := reports.List(context.Background())
	if len(stored) != 1 || stored[0].Status != feedback.StatusOpen {
		t.Fatalf("expected one stored open report, got %+v", stored)
	}
}

func TestFeedbackHandler_Submit_AttachesSignedInSubmitter(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := newSignedInUser(t, store, "feedback-submitter", "user", "approved")
	notifier := &fakeFeedbackNotifier{}
	reports := newFakeFeedbackStore()
	e := newFeedbackTestEcho(store, reports, notifier)

	rec := postFeedback(e, `{"kind":"suggestion","message":"Add dark mode."}`, cookie)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	want := "User feedback-submitter <feedback-submitter@example.com>"
	if notifier.got[0].SubmittedBy != want {
		t.Errorf("SubmittedBy = %q, want %q", notifier.got[0].SubmittedBy, want)
	}
	stored, _ := reports.List(context.Background())
	if len(stored) != 1 || stored[0].SubmittedByName != "User feedback-submitter" {
		t.Fatalf("expected stored report to carry the submitter's name, got %+v", stored)
	}
}

func TestFeedbackHandler_Submit_InvalidSessionCookieStillSucceeds(t *testing.T) {
	notifier := &fakeFeedbackNotifier{}
	e := newFeedbackTestEcho(newFakeUserStore(), newFakeFeedbackStore(), notifier)

	rec := postFeedback(e, `{"kind":"bug","message":"test"}`, &http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if notifier.got[0].SubmittedBy != "" {
		t.Errorf("SubmittedBy = %q, want empty for a bad session cookie", notifier.got[0].SubmittedBy)
	}
}

func TestFeedbackHandler_Submit_StoreErrorFailsTheRequest(t *testing.T) {
	notifier := &fakeFeedbackNotifier{}
	reports := newFakeFeedbackStore()
	reports.failErr = context.DeadlineExceeded
	e := newFeedbackTestEcho(newFakeUserStore(), reports, notifier)

	rec := postFeedback(e, `{"kind":"bug","message":"test"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d — a real persistence failure should fail the request", rec.Code, http.StatusInternalServerError)
	}
	if len(notifier.got) != 0 {
		t.Errorf("expected no NotifyFeedback call when the report was never saved, got %d", len(notifier.got))
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
			e := newFeedbackTestEcho(newFakeUserStore(), newFakeFeedbackStore(), notifier)

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
	e := newFeedbackTestEcho(newFakeUserStore(), newFakeFeedbackStore(), notifier)

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
	e := newFeedbackTestEcho(newFakeUserStore(), newFakeFeedbackStore(), notifier)

	rec := postFeedback(e, `{"kind":"bug","message":"test"}`)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d — a failed alert email should never fail the submission", rec.Code, http.StatusAccepted)
	}
}
