// Command server is the entry point for the teams-match-making backend
// API. Health checks, a database connection, the BCP proxy/cache
// (internal/bcp) that used to live entirely in the frontend, and now
// Google sign-in + server-side sessions (internal/auth, internal/user) —
// every browser shares this one server-side cache/rate limit against
// BCP, and every user's account lives here instead of per-browser
// localStorage. Follows/notes get built on top of this as the frontend's
// remaining TODO items land.
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/JaydeRussell/teams-match-making-be/internal/api"
	"github.com/JaydeRussell/teams-match-making-be/internal/applog"
	"github.com/JaydeRussell/teams-match-making-be/internal/auth"
	"github.com/JaydeRussell/teams-match-making-be/internal/bcp"
	"github.com/JaydeRussell/teams-match-making-be/internal/config"
	"github.com/JaydeRussell/teams-match-making-be/internal/db"
	"github.com/JaydeRussell/teams-match-making-be/internal/user"
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

	e := newServer(cfg, pool, bcp.NewClient(), logWriter)

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
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Output: logWriter,
		Format: `{"time":"${time_rfc3339}","level":"access","remote_ip":"${remote_ip}",` +
			`"method":"${method}","uri":"${uri}","status":${status},"error":"${error}",` +
			`"latency_human":"${latency_human}","bytes_in":${bytes_in},"bytes_out":${bytes_out},` +
			`"user_agent":"${user_agent}"}` + "\n",
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

	api.RegisterBCPRoutes(e, bcpClient)

	// Google sign-in is opt-in: only registered once real credentials
	// are configured, so the rest of this service (BCP proxy, health
	// checks) still runs before you've set up an OAuth client in Google
	// Cloud Console. See the README's "Google sign-in setup" section.
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		google := auth.NewGoogleOAuth(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL)
		userStore := user.NewStore(pool)
		api.RegisterAuthRoutes(e, google, userStore, cfg.FrontendBaseURL, cfg.CookieSecure)
		// The BCP-profile-link + "my events" routes are session-gated (see
		// internal/api/me.go), so there's no point registering them
		// without sign-in itself also being enabled.
		api.RegisterMeRoutes(e, userStore, bcpClient)
		// Cross-device follows/recent-events sync — also session-gated,
		// so it only makes sense once sign-in itself is enabled.
		api.RegisterSyncRoutes(e, userStore)
		log.Printf("Google sign-in enabled (redirect URL: %s)", cfg.GoogleRedirectURL)
	} else {
		log.Printf("Google sign-in disabled: GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET not set (see .env.example)")
	}

	return e
}
