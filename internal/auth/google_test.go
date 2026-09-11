package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewState(t *testing.T) {
	seen := make(map[string]bool)
	for i := range 20 {
		state, err := NewState()
		if err != nil {
			t.Fatalf("NewState() returned error: %v", err)
		}
		if state == "" {
			t.Fatal("NewState() returned an empty string")
		}
		if seen[state] {
			t.Fatalf("NewState() returned a duplicate value %q across %d calls", state, i+1)
		}
		seen[state] = true
	}
}

func TestAuthCodeURL(t *testing.T) {
	g := NewGoogleOAuth("client-123", "secret-abc", "https://app.example.com/auth/google/callback")
	got := g.AuthCodeURL("state-xyz")

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("AuthCodeURL returned an unparseable URL %q: %v", got, err)
	}
	if !strings.HasPrefix(got, defaultAuthURL) {
		t.Errorf("AuthCodeURL = %q, want it to start with %q", got, defaultAuthURL)
	}

	q := parsed.Query()
	cases := []struct {
		param string
		want  string
	}{
		{"client_id", "client-123"},
		{"redirect_uri", "https://app.example.com/auth/google/callback"},
		{"response_type", "code"},
		{"scope", "openid email profile"},
		{"state", "state-xyz"},
	}
	for _, tc := range cases {
		if got := q.Get(tc.param); got != tc.want {
			t.Errorf("query param %q = %q, want %q", tc.param, got, tc.want)
		}
	}
	// The client secret must never appear in a URL the browser is sent to.
	if strings.Contains(got, "secret-abc") {
		t.Errorf("AuthCodeURL leaked the client secret into the URL: %q", got)
	}
}

// tokenAndUserInfoServer stands up one stub server handling both the
// token endpoint and the userinfo endpoint at distinct paths, since a
// real GoogleOAuth exercises them in sequence (Exchange, then
// FetchUserInfo). tokenHandler/userInfoHandler let each test case
// control both responses independently.
func newTestClient(t *testing.T, tokenHandler, userInfoHandler http.HandlerFunc) *GoogleOAuth {
	t.Helper()
	mux := http.NewServeMux()
	if tokenHandler != nil {
		mux.HandleFunc("/token", tokenHandler)
	}
	if userInfoHandler != nil {
		mux.HandleFunc("/userinfo", userInfoHandler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return NewGoogleOAuthWithBaseURLs(
		"client-123", "secret-abc", "https://app.example.com/callback",
		server.URL+"/auth", server.URL+"/token", server.URL+"/userinfo",
	)
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestExchange(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		body           string
		wantErr        bool
		wantErrContain string
		wantToken      string
	}{
		{
			name:      "successful exchange returns the access token",
			status:    http.StatusOK,
			body:      `{"access_token": "tok-abc", "token_type": "Bearer", "expires_in": 3600}`,
			wantToken: "tok-abc",
		},
		{
			name:           "an error response surfaces Google's error_description",
			status:         http.StatusBadRequest,
			body:           `{"error": "invalid_grant", "error_description": "Malformed auth code."}`,
			wantErr:        true,
			wantErrContain: "Malformed auth code.",
		},
		{
			name:           "an error response with no description falls back to the error code",
			status:         http.StatusBadRequest,
			body:           `{"error": "invalid_grant"}`,
			wantErr:        true,
			wantErrContain: "invalid_grant",
		},
		{
			name:           "a non-2xx response with an unparseable/empty error body falls back to the status",
			status:         http.StatusInternalServerError,
			body:           `{}`,
			wantErr:        true,
			wantErrContain: "500",
		},
		{
			name:           "a 2xx response with no access_token is still an error",
			status:         http.StatusOK,
			body:           `{"token_type": "Bearer"}`,
			wantErr:        true,
			wantErrContain: "no access token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, jsonHandler(tc.status, tc.body), nil)
			token, err := client.Exchange(context.Background(), "auth-code-123")

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Exchange succeeded with token %q, want an error", token)
				}
				if !strings.Contains(err.Error(), tc.wantErrContain) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErrContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exchange returned error: %v", err)
			}
			if token != tc.wantToken {
				t.Errorf("Exchange = %q, want %q", token, tc.wantToken)
			}
		})
	}
}

func TestExchange_SendsExpectedFormFields(t *testing.T) {
	var gotBody string
	var gotContentType string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token": "tok"}`))
	}, nil)

	if _, err := client.Exchange(context.Background(), "the-code"); err != nil {
		t.Fatalf("Exchange returned error: %v", err)
	}

	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotContentType)
	}
	form, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("couldn't parse request body %q as a form: %v", gotBody, err)
	}
	cases := map[string]string{
		"client_id":     "client-123",
		"client_secret": "secret-abc",
		"code":          "the-code",
		"redirect_uri":  "https://app.example.com/callback",
		"grant_type":    "authorization_code",
	}
	for field, want := range cases {
		if got := form.Get(field); got != want {
			t.Errorf("form field %q = %q, want %q", field, got, want)
		}
	}
}

func TestFetchUserInfo(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		body           string
		wantErr        bool
		wantErrContain string
		want           UserInfo
	}{
		{
			name:   "a full profile is returned as-is",
			status: http.StatusOK,
			body:   `{"sub": "1234567890", "email": "player@example.com", "name": "Anna Adams", "picture": "https://example.com/pic.jpg"}`,
			want:   UserInfo{Sub: "1234567890", Email: "player@example.com", Name: "Anna Adams", Picture: "https://example.com/pic.jpg"},
		},
		{
			name:           "a non-2xx response is an error",
			status:         http.StatusUnauthorized,
			body:           `{"error": "invalid_token"}`,
			wantErr:        true,
			wantErrContain: "401",
		},
		{
			name:           "a response missing sub is an error, even if otherwise well-formed",
			status:         http.StatusOK,
			body:           `{"email": "player@example.com", "name": "Anna Adams"}`,
			wantErr:        true,
			wantErrContain: "sub",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, nil, jsonHandler(tc.status, tc.body))
			info, err := client.FetchUserInfo(context.Background(), "access-token-abc")

			if tc.wantErr {
				if err == nil {
					t.Fatalf("FetchUserInfo succeeded with %+v, want an error", info)
				}
				if !strings.Contains(err.Error(), tc.wantErrContain) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErrContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchUserInfo returned error: %v", err)
			}
			if info != tc.want {
				t.Errorf("FetchUserInfo = %+v, want %+v", info, tc.want)
			}
		})
	}
}

func TestFetchUserInfo_SendsBearerToken(t *testing.T) {
	var gotAuthHeader string
	client := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub": "1"}`))
	})

	if _, err := client.FetchUserInfo(context.Background(), "my-access-token"); err != nil {
		t.Fatalf("FetchUserInfo returned error: %v", err)
	}
	if gotAuthHeader != "Bearer my-access-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuthHeader, "Bearer my-access-token")
	}
}
