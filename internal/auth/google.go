// Package auth implements "Sign in with Google" and this service's
// server-side sessions. Deliberately hand-rolled rather than pulling in
// golang.org/x/oauth2 (or a JWT/JWK library to verify Google's ID
// token): the standard authorization-code flow for one provider is a
// handful of HTTP calls, and fetching the account profile from Google's
// userinfo endpoint (over TLS, straight from Google) is just as
// trustworthy here as decoding and verifying a signed ID token, without
// the extra dependency.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL    = "https://oauth2.googleapis.com/token"
	defaultUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

// GoogleOAuth is this service's client for Google's OAuth2
// authorization-code flow: build the consent-screen redirect URL,
// exchange the code the callback receives for an access token, and
// fetch the signed-in account's profile.
type GoogleOAuth struct {
	http *http.Client

	// Endpoint URLs — fields rather than package consts so tests can
	// point a GoogleOAuth at a stub server instead of the real Google
	// API. NewGoogleOAuth sets these to the real defaults above.
	authURL     string
	tokenURL    string
	userInfoURL string

	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// NewGoogleOAuth builds a client pointed at the real Google endpoints.
func NewGoogleOAuth(clientID, clientSecret, redirectURL string) *GoogleOAuth {
	return newGoogleOAuthWithBaseURLs(clientID, clientSecret, redirectURL, defaultAuthURL, defaultTokenURL, defaultUserInfoURL)
}

// NewGoogleOAuthWithBaseURLs builds a client against arbitrary endpoint
// URLs — for tests (in this package or elsewhere) that stand up a stub
// HTTP server rather than reaching the real Google API.
func NewGoogleOAuthWithBaseURLs(clientID, clientSecret, redirectURL, authURL, tokenURL, userInfoURL string) *GoogleOAuth {
	return newGoogleOAuthWithBaseURLs(clientID, clientSecret, redirectURL, authURL, tokenURL, userInfoURL)
}

func newGoogleOAuthWithBaseURLs(clientID, clientSecret, redirectURL, authURL, tokenURL, userInfoURL string) *GoogleOAuth {
	return &GoogleOAuth{
		http:         &http.Client{Timeout: 10 * time.Second},
		authURL:      authURL,
		tokenURL:     tokenURL,
		userInfoURL:  userInfoURL,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
	}
}

// NewState returns a fresh, unguessable random string for OAuth's
// "state" parameter — CSRF protection for the redirect flow. The caller
// stores this in a short-lived cookie before redirecting to Google, and
// must check that the callback's state query param matches it before
// trusting the callback (its code) at all.
func NewState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthCodeURL builds the URL to send the browser to, to start Google's
// consent screen. state should be a value from NewState.
func (g *GoogleOAuth) AuthCodeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	// Without this, a user who's already granted consent before skips
	// straight back with no chance to pick a different Google account.
	q.Set("prompt", "select_account")
	return g.authURL + "?" + q.Encode()
}

// tokenResponse is the subset of Google's token-endpoint response (both
// success and error shapes) this service actually uses.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// Exchange trades an authorization code (the callback's ?code= query
// param) for an access token good for calling FetchUserInfo.
func (g *GoogleOAuth) Exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", g.RedirectURL)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting token: %w", err)
	}
	defer res.Body.Close()

	var body tokenResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg := body.ErrorDesc
		if msg == "" {
			msg = body.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return "", fmt.Errorf("token exchange failed: %s", msg)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("token exchange succeeded but returned no access token")
	}

	return body.AccessToken, nil
}

// UserInfo is the subset of Google's userinfo response this app
// actually uses. Sub is Google's stable per-account id — what
// identifies a returning user across sign-ins, since (unlike email) it
// can't change.
type UserInfo struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// FetchUserInfo returns the signed-in Google account's profile for a
// valid access token from Exchange.
func (g *GoogleOAuth) FetchUserInfo(ctx context.Context, accessToken string) (UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userInfoURL, nil)
	if err != nil {
		return UserInfo{}, fmt.Errorf("building userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	res, err := g.http.Do(req)
	if err != nil {
		return UserInfo{}, fmt.Errorf("requesting userinfo: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return UserInfo{}, fmt.Errorf("userinfo request failed: HTTP %d", res.StatusCode)
	}

	var info UserInfo
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		return UserInfo{}, fmt.Errorf("decoding userinfo response: %w", err)
	}
	if info.Sub == "" {
		return UserInfo{}, fmt.Errorf("userinfo response missing sub (account id)")
	}

	return info, nil
}
