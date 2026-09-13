// Package config loads this service's runtime configuration from
// environment variables (with an optional local .env file for
// development — see main.go). Kept deliberately small: as the backend
// grows (auth, sessions, etc.) new settings get a field here rather than
// being read from os.Getenv scattered throughout the codebase.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds everything the server needs to start.
type Config struct {
	// Port is the local port the HTTP server listens on. Defaults to
	// 8080 if PORT isn't set, which is the convention most PaaS hosts
	// (Fly.io, Railway, Render) expect for a service that receives
	// external traffic on a platform-assigned port.
	Port string

	// DatabaseURL is a standard Postgres connection string, e.g.
	// postgres://user:password@host:5432/dbname?sslmode=require — the
	// exact format a managed provider (Neon, Supabase, Railway) hands
	// you when you create a database. Required: this service has
	// nothing meaningful to do without a database once user data exists,
	// so it fails fast at startup rather than serving requests it can't
	// actually fulfill.
	DatabaseURL string

	// GoogleClientID and GoogleClientSecret come from a Google Cloud
	// Console OAuth client (see the README's "Running locally" section).
	// Deliberately NOT required: leaving them unset
	// lets the rest of the service run — the BCP proxy, health checks —
	// before Google sign-in is set up, rather than refusing to start at
	// all. main.go only registers the /auth/* and /api/me routes when
	// both are present.
	GoogleClientID     string
	GoogleClientSecret string

	// GoogleRedirectURL is where Google sends the browser back after
	// consent — it must be registered on the OAuth client in Google
	// Cloud Console exactly. Defaults to this backend's own
	// conventional local address; override it for anything other than
	// local development.
	GoogleRedirectURL string

	// FrontendBaseURL is where a sign-in (successful or not) redirects
	// the browser back to.
	FrontendBaseURL string

	// CookieSecure controls the session/state cookies' Secure flag.
	// Leave this false for local http://localhost development — a
	// browser silently refuses to store a Secure cookie at all over
	// plain HTTP — and set COOKIE_SECURE=true for any real (HTTPS)
	// deployment.
	CookieSecure bool

	// LogFile is where this service's logs (Echo's per-request access
	// log, plus its own log.Printf/log.Fatalf calls) are written, in
	// addition to always being written to stdout too — see
	// internal/applog. Defaults to "logs/backend.log", a path relative
	// to the working directory the server was started from. Explicitly
	// setting LOG_FILE="" (as opposed to leaving it unset) logs to
	// stdout only.
	LogFile string

	// AdminEmails bootstraps access control (see migration 0007 and
	// internal/api/admin.go): every sign-in whose email matches one of
	// these (case-insensitively) is auto-approved as an admin, every
	// time they sign in — not just their first. Without this, a
	// pending-by-default new deployment would have no way to ever
	// approve its very first user, including its own operator. Comma-
	// separated, e.g. ADMIN_EMAILS=you@example.com,other@example.com.
	// Leaving it unset is valid (every sign-in just starts pending, and
	// stays that way until promoted some other way — e.g. directly in
	// the database), but means nobody can approve anybody.
	AdminEmails []string

	// ResendAPIKey and EmailFromAddress enable emailing every AdminEmails
	// address when a brand-new account signs up pending approval (see
	// internal/notify and internal/api/auth.go's Callback). Both
	// optional, same "leaving it unset just disables the feature"
	// contract as GoogleClientID/GoogleClientSecret above — main.go only
	// builds a working notifier when both are set; a deployment without
	// Resend configured still starts and runs fine, admins just have to
	// keep checking /admin manually.
	ResendAPIKey     string
	EmailFromAddress string
}

// Load reads configuration from the environment. Returns an error if a
// required value is missing, rather than silently starting in a broken
// state.
func Load() (Config, error) {
	cfg := Config{
		Port:               getEnvOrDefault("PORT", "8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:  getEnvOrDefault("GOOGLE_REDIRECT_URL", "http://localhost:8080/auth/google/callback"),
		FrontendBaseURL:    getEnvOrDefault("FRONTEND_BASE_URL", "http://localhost:3000"),
		CookieSecure:       os.Getenv("COOKIE_SECURE") == "true",
		LogFile:            getEnvOrDefaultAllowingEmpty("LOG_FILE", "logs/backend.log"),
		AdminEmails:        parseAdminEmails(os.Getenv("ADMIN_EMAILS")),
		ResendAPIKey:       os.Getenv("RESEND_API_KEY"),
		EmailFromAddress:   os.Getenv("EMAIL_FROM_ADDRESS"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required (see .env.example)")
	}

	return cfg, nil
}

// parseAdminEmails splits a comma-separated ADMIN_EMAILS value into
// trimmed, lowercased addresses (matching is case-insensitive, since
// that's how email addresses behave in practice), silently dropping any
// empty entries an accidental trailing/leading/double comma would
// otherwise produce. Returns nil (not an error) for an unset/empty
// value — see AdminEmails' doc comment for what that means.
func parseAdminEmails(raw string) []string {
	if raw == "" {
		return nil
	}
	var emails []string
	for part := range strings.SplitSeq(raw, ",") {
		email := strings.ToLower(strings.TrimSpace(part))
		if email != "" {
			emails = append(emails, email)
		}
	}
	return emails
}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvOrDefaultAllowingEmpty is like getEnvOrDefault, but distinguishes
// "unset" from "explicitly set to an empty string" — used for LOG_FILE,
// where an explicit empty value has a real meaning (stdout-only
// logging) that plain getEnvOrDefault's "empty means missing" behavior
// would otherwise silently override back to the default.
func getEnvOrDefaultAllowingEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
