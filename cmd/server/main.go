// Command server is the entry point for the brass-ledger backend
// API. Health checks, a database connection, the BCP proxy/cache
// (internal/bcp) that used to live entirely in the frontend, and now
// Google sign-in + server-side sessions (internal/auth, internal/user) —
// every browser shares this one server-side cache/rate limit against
// BCP, and every user's account lives here instead of per-browser
// localStorage.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/JaydeRussell/brass-ledger-api/internal/api"
	"github.com/JaydeRussell/brass-ledger-api/internal/applog"
	"github.com/JaydeRussell/brass-ledger-api/internal/auth"
	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/bcpcache"
	"github.com/JaydeRussell/brass-ledger-api/internal/config"
	"github.com/JaydeRussell/brass-ledger-api/internal/db"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

func main() {
	// Loads a local .env file if one exists (for `go run` during
	// development) — silently does nothing if it's missing, since a real
	// deployment sets these as actual environment variables instead. See
	// .env.example for what to put in it.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Every log.Printf/log.Fatalf from here on (in this file and any
	// package that uses the standard log package — internal/api's auth
	// routes included) goes to both stdout and cfg.LogFile, so a single
	// file has the full startup + request + error history to read back
	// later. See internal/applog and LOG_FILE in .env.example.
	logWriter, err := applog.Open(cfg.LogFile)
	if err != nil {
		log.Fatalf("logging: %v", err)
	}
	defer logWriter.Close()
	log.SetOutput(logWriter)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if cfg.LogFile != "" {
		log.Printf("logging to %s (and stdout)", cfg.LogFile)
	}

	ctx := context.Background()
	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("running database migrations: %v", err)
	}

	bcpClient := bcp.NewClient()
	// Persists the subset of BCP responses that are genuinely immutable
	// (an already-concluded event's info/roster/pairings/placings, and
	// league metadata) so they're fetched from BCP once, ever, rather
	// than every 60 seconds by every visitor forever — see
	// internal/bcp/durable.go and internal/bcpcache.
	bcpClient.SetDurableCache(bcpcache.New(pool))
	e := newServer(cfg, pool, bcpClient, logWriter)

	go func() {
		if err := e.Start(":" + cfg.Port); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()
	log.Printf("listening on :%s", cfg.Port)

	// Graceful shutdown: let in-flight requests finish (up to 10s) rather
	// than dropping them when the process is stopped/redeployed.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}

// newServer wires up the Echo instance and routes. Split out from main
// so it can be exercised directly in tests later without also starting a
// real listener.
//
// logWriter receives Echo's own startup/error logging and its
// per-request access log (method, path, status, latency, etc. as one
// JSON line per request) — the same destination main.go pointed the
// standard log package at, so a request and whatever internal/api logs
// about handling it end up interleaved in one file in the order they
// actually happened.
func newServer(cfg config.Config, pool *pgxpool.Pool, bcpClient *bcp.Client, logWriter io.Writer) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Logger.SetOutput(logWriter)
	// middleware.LoggerWithConfig's template-string API is deprecated as of
	// Echo v4.15 in favor of this callback-based one — same JSON shape as
	// before (built with encoding/json here instead of raw template
	// substitution, so field values that happen to contain a `"` no
	// longer produce invalid JSON, a bug the old template approach had).
	// HandleError is deliberately left false: the old logger never called
	// the global error handler itself either, just logged whatever error
	// came back — Echo's own router already invokes it exactly once after
	// the full middleware chain returns, same as before this migration.
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogRemoteIP:      true,
		LogMethod:        true,
		LogURI:           true,
		LogStatus:        true,
		LogError:         true,
		LogLatency:       true,
		LogContentLength: true,
		LogResponseSize:  true,
		LogUserAgent:     true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			errMsg := ""
			if v.Error != nil {
				errMsg = v.Error.Error()
			}
			// v.ContentLength is the raw Content-Length header value
			// (empty if the request didn't send one) — parsed to a number
			// to match the old template's `${bytes_in}` output shape.
			bytesIn, _ := strconv.ParseInt(v.ContentLength, 10, 64)
			line, err := json.Marshal(struct {
				Time         string `json:"time"`
				Level        string `json:"level"`
				RemoteIP     string `json:"remote_ip"`
				Method       string `json:"method"`
				URI          string `json:"uri"`
				Status       int    `json:"status"`
				Error        string `json:"error"`
				LatencyHuman string `json:"latency_human"`
				BytesIn      int64  `json:"bytes_in"`
				BytesOut     int64  `json:"bytes_out"`
				UserAgent    string `json:"user_agent"`
			}{
				Time:         v.StartTime.Format(time.RFC3339),
				Level:        "access",
				RemoteIP:     v.RemoteIP,
				Method:       v.Method,
				URI:          v.URI,
				Status:       v.Status,
				Error:        errMsg,
				LatencyHuman: v.Latency.String(),
				BytesIn:      bytesIn,
				BytesOut:     v.ResponseSize,
				UserAgent:    v.UserAgent,
			})
			if err != nil {
				return err
			}
			_, err = logWriter.Write(append(line, '\n'))
			return err
		},
	}))
	e.Use(middleware.Recover())
	// Scoped to the actual frontend origin, with credentials allowed —
	// required for the session cookie Google sign-in sets to actually
	// reach this API from the browser at all: a cookie's "same-site"
	// rules only permit that combination for one named origin, never a
	// wildcard (`AllowOrigins: []string{"*"}` and AllowCredentials can't
	// be used together — browsers reject that combination outright).
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     []string{cfg.FrontendBaseURL},
		AllowCredentials: true,
	}))

	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// /readyz additionally checks the database is actually reachable —
	// useful for a deploy platform's health check to catch "the process
	// is up but can't do anything real yet" separately from /healthz.
	e.GET("/readyz", func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"error":  err.Error(),
			})
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Built unconditionally (not just when Google sign-in is configured)
	// since api.RequireApproved — gating BCPHandler's routes below —
	// needs it regardless. It's a thin wrapper around pool, so
	// constructing it has no cost or side effect on its own.
	userStore := user.NewStore(pool)
	api.NewBCPHandler(bcpClient).Register(e, api.RequireApproved(userStore), api.RequireSession(userStore))

	// Google sign-in is opt-in: only registered once real credentials
	// are configured. Note that means this whole app — not just the
	// account-specific features below — is unreachable without it now:
	// BCPHandler's routes above require an approved session, and without
	// Google sign-in registered there's no way to ever get one. See the
	// README's "Google sign-in setup" section.
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		google := auth.NewGoogleOAuth(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL)
		api.NewAuthHandler(google, userStore, cfg.FrontendBaseURL, cfg.CookieSecure, cfg.AdminEmails).Register(e)
		// The BCP-profile-link + "my events" routes are session-gated (see
		// internal/api/me.go), so there's no point registering them
		// without sign-in itself also being enabled.
		api.NewMeHandler(userStore, bcpClient).Register(e)
		// Cross-device follows/recent-events sync — also session-gated,
		// so it only makes sense once sign-in itself is enabled.
		api.NewSyncHandler(userStore).Register(e)
		// Player stats summary (best placing, faction breakdown) — same
		// session gating, built on the same BCP data "my events" already
		// fetches.
		api.NewStatsHandler(userStore, bcpClient).Register(e)
		// Access-control management (migration 0007) — admin-only, same
		// reason it only makes sense once sign-in itself is enabled.
		api.NewAdminHandler(userStore).Register(e)
		log.Printf("Google sign-in enabled (redirect URL: %s)", cfg.GoogleRedirectURL)
		if len(cfg.AdminEmails) == 0 {
			log.Printf("ADMIN_EMAILS is not set — nobody can approve a pending account (see .env.example)")
		}
	} else {
		log.Printf("Google sign-in disabled: GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET not set (see .env.example) — every route except /healthz and /readyz will 401, since there's no way to get a session")
	}

	return e
}
