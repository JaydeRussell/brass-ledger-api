package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

func TestResendNotifier_NotifyNewSignup(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody resendEmailRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewResendNotifierWithBaseURL(server.URL, "test-key", "alerts@example.com", []string{"admin1@example.com", "admin2@example.com"}, "https://brass-ledger.app/admin")

	err := n.NotifyNewSignup(context.Background(), user.User{Name: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("NotifyNewSignup: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/emails" {
		t.Errorf("path = %q, want /emails", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotBody.From != "alerts@example.com" {
		t.Errorf("from = %q, want alerts@example.com", gotBody.From)
	}
	if len(gotBody.To) != 2 || gotBody.To[0] != "admin1@example.com" || gotBody.To[1] != "admin2@example.com" {
		t.Errorf("to = %v, want [admin1@example.com admin2@example.com]", gotBody.To)
	}
	if gotBody.Subject == "" || gotBody.Text == "" {
		t.Errorf("expected non-empty subject and text, got %+v", gotBody)
	}
}

func TestResendNotifier_NotifyNewSignup_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	n := NewResendNotifierWithBaseURL(server.URL, "test-key", "alerts@example.com", []string{"admin@example.com"}, "https://brass-ledger.app/admin")

	if err := n.NotifyNewSignup(context.Background(), user.User{Name: "Alice", Email: "alice@example.com"}); err == nil {
		t.Fatal("expected an error on a 500 response, got nil")
	}
}

func TestResendNotifier_NotifyNewSignup_Disabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request should be made when the notifier is disabled")
	}))
	defer server.Close()

	cases := []struct {
		name   string
		apiKey string
		from   string
		to     []string
	}{
		{"no API key", "", "alerts@example.com", []string{"admin@example.com"}},
		{"no from address", "test-key", "", []string{"admin@example.com"}},
		{"no recipients", "test-key", "alerts@example.com", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := NewResendNotifierWithBaseURL(server.URL, tc.apiKey, tc.from, tc.to, "https://brass-ledger.app/admin")
			if err := n.NotifyNewSignup(context.Background(), user.User{Name: "Alice", Email: "alice@example.com"}); err != nil {
				t.Fatalf("NotifyNewSignup (disabled): %v", err)
			}
		})
	}
}
