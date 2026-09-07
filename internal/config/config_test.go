package config

import (
	"os"
	"testing"
)

// TestLoad covers every branch of Load(): the required DATABASE_URL
// check, and PORT's default-vs-explicit behavior. Each case sets up its
// own environment (via t.Setenv, which restores the previous value
// automatically at the end of the subtest) and checks the result against
// what's expected — the table-driven shape for a function whose behavior
// depends on external state rather than just its arguments.
func TestLoad(t *testing.T) {
	cases := []struct {
		name        string
		databaseURL string // "" means: don't set DATABASE_URL at all
		port        string // "" means: don't set PORT at all
		wantErr     bool
		wantPort    string
	}{
		{
			name:        "missing DATABASE_URL is an error",
			databaseURL: "",
			wantErr:     true,
		},
		{
			name:        "DATABASE_URL set, PORT unset defaults to 8080",
			databaseURL: "postgres://user:pass@localhost:5432/db?sslmode=disable",
			port:        "",
			wantPort:    "8080",
		},
		{
			name:        "DATABASE_URL and PORT both set are both respected",
			databaseURL: "postgres://user:pass@localhost:5432/db?sslmode=disable",
			port:        "9090",
			wantPort:    "9090",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv can't unset a var (it always sets, even to ""), but
			// Load() specifically checks for an empty DATABASE_URL, so
			// setting "" here exercises the same branch as truly missing.
			t.Setenv("DATABASE_URL", tc.databaseURL)
			t.Setenv("PORT", tc.port)

			cfg, err := Load()

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded with cfg %+v, want an error", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			if cfg.DatabaseURL != tc.databaseURL {
				t.Errorf("cfg.DatabaseURL = %q, want %q", cfg.DatabaseURL, tc.databaseURL)
			}
			if cfg.Port != tc.wantPort {
				t.Errorf("cfg.Port = %q, want %q", cfg.Port, tc.wantPort)
			}
		})
	}
}

// TestLoad_GoogleAndCookieDefaults covers the fields Google sign-in
// added: GoogleClientID/Secret are optional (never fail Load on their
// own), GoogleRedirectURL/FrontendBaseURL fall back to conventional
// local-dev defaults, and CookieSecure parses "true" strictly (anything
// else, including unset, means false — the safe default for local
// http://localhost development, where a Secure cookie wouldn't even be
// stored).
func TestLoad_GoogleAndCookieDefaults(t *testing.T) {
	cases := []struct {
		name               string
		googleClientID     string
		googleClientSecret string
		googleRedirectURL  string
		frontendBaseURL    string
		cookieSecure       string
		wantRedirectURL    string
		wantFrontendURL    string
		wantCookieSecure   bool
	}{
		{
			name:             "nothing set: Google unset, sensible local defaults, cookies not secure",
			wantRedirectURL:  "http://localhost:8080/auth/google/callback",
			wantFrontendURL:  "http://localhost:3000",
			wantCookieSecure: false,
		},
		{
			name:              "explicit redirect/frontend URLs are respected",
			googleRedirectURL: "https://api.example.com/auth/google/callback",
			frontendBaseURL:   "https://app.example.com",
			wantRedirectURL:   "https://api.example.com/auth/google/callback",
			wantFrontendURL:   "https://app.example.com",
		},
		{
			name:             `COOKIE_SECURE=true enables it`,
			cookieSecure:     "true",
			wantRedirectURL:  "http://localhost:8080/auth/google/callback",
			wantFrontendURL:  "http://localhost:3000",
			wantCookieSecure: true,
		},
		{
			name:             "any other COOKIE_SECURE value is treated as false, not an error",
			cookieSecure:     "yes please",
			wantRedirectURL:  "http://localhost:8080/auth/google/callback",
			wantFrontendURL:  "http://localhost:3000",
			wantCookieSecure: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db?sslmode=disable")
			t.Setenv("GOOGLE_CLIENT_ID", tc.googleClientID)
			t.Setenv("GOOGLE_CLIENT_SECRET", tc.googleClientSecret)
			t.Setenv("GOOGLE_REDIRECT_URL", tc.googleRedirectURL)
			t.Setenv("FRONTEND_BASE_URL", tc.frontendBaseURL)
			t.Setenv("COOKIE_SECURE", tc.cookieSecure)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned error: %v (Google credentials must never be required)", err)
			}

			if cfg.GoogleClientID != tc.googleClientID {
				t.Errorf("cfg.GoogleClientID = %q, want %q", cfg.GoogleClientID, tc.googleClientID)
			}
			if cfg.GoogleClientSecret != tc.googleClientSecret {
				t.Errorf("cfg.GoogleClientSecret = %q, want %q", cfg.GoogleClientSecret, tc.googleClientSecret)
			}
			if cfg.GoogleRedirectURL != tc.wantRedirectURL {
				t.Errorf("cfg.GoogleRedirectURL = %q, want %q", cfg.GoogleRedirectURL, tc.wantRedirectURL)
			}
			if cfg.FrontendBaseURL != tc.wantFrontendURL {
				t.Errorf("cfg.FrontendBaseURL = %q, want %q", cfg.FrontendBaseURL, tc.wantFrontendURL)
			}
			if cfg.CookieSecure != tc.wantCookieSecure {
				t.Errorf("cfg.CookieSecure = %v, want %v", cfg.CookieSecure, tc.wantCookieSecure)
			}
		})
	}
}

// TestLoad_LogFile covers LOG_FILE's three-way behavior: left unset, it
// defaults to "logs/backend.log"; explicitly set to a path, that path
// is respected; and explicitly set to an empty string, that empty
// string is respected too (meaning stdout-only logging — see
// internal/applog) rather than falling back to the default the way
// every other getEnvOrDefault-backed field would. t.Setenv can't
// represent "truly unset" (it always sets, even to ""), so the unset
// case uses os.Unsetenv directly with a manual restore instead.
func TestLoad_LogFile(t *testing.T) {
	cases := []struct {
		name        string
		setLogFile  bool // false: leave LOG_FILE unset entirely
		logFile     string
		wantLogFile string
	}{
		{
			name:        "unset defaults to logs/backend.log",
			setLogFile:  false,
			wantLogFile: "logs/backend.log",
		},
		{
			name:        "explicit path is respected",
			setLogFile:  true,
			logFile:     "/var/log/tmm/backend.log",
			wantLogFile: "/var/log/tmm/backend.log",
		},
		{
			name:        "explicit empty string means stdout-only, not the default",
			setLogFile:  true,
			logFile:     "",
			wantLogFile: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db?sslmode=disable")

			original, hadOriginal := os.LookupEnv("LOG_FILE")
			t.Cleanup(func() {
				if hadOriginal {
					os.Setenv("LOG_FILE", original)
				} else {
					os.Unsetenv("LOG_FILE")
				}
			})

			if tc.setLogFile {
				os.Setenv("LOG_FILE", tc.logFile)
			} else {
				os.Unsetenv("LOG_FILE")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			if cfg.LogFile != tc.wantLogFile {
				t.Errorf("cfg.LogFile = %q, want %q", cfg.LogFile, tc.wantLogFile)
			}
		})
	}
}
