# Brass Ledger

[![CI/CD](https://github.com/JaydeRussell/brass-ledger-api/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/JaydeRussell/brass-ledger-api/actions/workflows/ci.yml)

**Tournament Companion** for Warhammer 40k — this is the backend for
[brass-ledger-web](../brass-ledger-web). Written in Go, using
[Echo](https://echo.labstack.com/) and Postgres.

This service does three things:

- **Proxies and caches Best Coast Pairings' (BCP) data** — event info,
  rosters, pairings, placings, ITC rankings (`internal/bcp`, routed in
  `internal/api/bcp.go`). This logic used to live entirely in the
  frontend, with every browser tab fetching from BCP and rate-limiting
  itself independently; it's moved here so every user of this app shares
  one server-side cache and one rate limit against BCP instead. The
  frontend's `app/lib/bcp.ts` is now a thin client that calls this
  service instead of BCP directly — set `NEXT_PUBLIC_BACKEND_URL` there
  (see its `.env.example`) to point at wherever this backend runs.
- **Google sign-in and server-side sessions** — accounts live here
  (`internal/auth`, `internal/user`, routed in `internal/api/auth.go`),
  not per-browser `localStorage`, so they follow a user across devices.
  See "Google sign-in setup" below to configure it.
- **Will hold cross-device follows/notes** once accounts exist to attach
  them to — the frontend's remaining TODO items build on top of this.

Same scope limit as the frontend: this only ever retrieves data BCP
already publishes. It never computes, ranks, or suggests a pairing/matchup
of any kind — see the doc comment at the top of `internal/bcp/types.go`.

## Requirements

- Go 1.22+
- A Postgres database — a free tier from [Neon](https://neon.tech) or
  [Supabase](https://supabase.com) works fine for development; no local
  Postgres install required unless you'd rather run one.

## Running locally

```bash
cp .env.example .env   # fill in DATABASE_URL (and Google sign-in vars — see below)
go mod tidy             # fetches dependencies, generates go.sum
go run ./cmd/server
```

Then:

```bash
curl localhost:8080/healthz   # {"status":"ok"} once the process is up
curl localhost:8080/readyz    # also checks the database is reachable
```

On startup the server also applies any pending database migrations
(`internal/db/migrate.go`) automatically — there's no separate migrate
command to remember to run.

## Google sign-in setup

Google sign-in is **optional** — leave `GOOGLE_CLIENT_ID` /
`GOOGLE_CLIENT_SECRET` unset in `.env` and everything else (the BCP
proxy, health checks) still works; the server just logs that sign-in is
disabled and skips registering the `/auth/*` and `/api/me` routes. To
turn it on, you need an OAuth client from Google Cloud Console — Google
doesn't let an app create this for you, so it's a one-time manual setup:

1. Go to [console.cloud.google.com](https://console.cloud.google.com/)
   and sign in with any Google account (this can be a brand new,
   personal project — it doesn't need to belong to an organization).
2. Create a new project (top-left project picker → "New Project"). Any
   name works, e.g. "Brass Ledger".
3. In the left sidebar, go to **APIs & Services → OAuth consent
   screen**.
   - User type: **External** (unless you have a Google Workspace
     organization and only ever plan to sign in with accounts from it —
     External is right for personal/hobby use).
   - Fill in the required fields (app name, your email as the support
     and developer contact). You can leave the app in **Testing**
     status — that's fine for personal use; it just means only email
     addresses you explicitly add as test users can sign in, and
     Google shows an "unverified app" warning screen (click "Advanced →
     Go to [app name] (unsafe)" to get past it — this is expected for
     an app that isn't published/verified, not a sign of anything being
     misconfigured).
   - Under **Audience** (or **Test users**, depending on the current
     console layout), add your own Google account's email address as a
     test user — otherwise Google will refuse to let even you sign in.
4. Go to **APIs & Services → Credentials → Create Credentials → OAuth
   client ID**.
   - Application type: **Web application**.
   - Name: anything, e.g. "Brass Ledger (local)".
   - **Authorized redirect URIs**: add exactly
     `http://localhost:8080/auth/google/callback` (or whatever you set
     `GOOGLE_REDIRECT_URL` to below — it must match byte-for-byte,
     including the path). You don't need to add an "Authorized
     JavaScript origin" — this app's OAuth flow is a server-side
     redirect, not a client-side Google Sign-In button.
5. Click **Create**. Google shows you a **Client ID** and **Client
   Secret** — copy both into your `.env`:
   ```bash
   GOOGLE_CLIENT_ID=your-client-id.apps.googleusercontent.com
   GOOGLE_CLIENT_SECRET=your-client-secret
   ```
6. Restart the server (`go run ./cmd/server`, or `make docker-reset` if
   you're running the Docker stack). Its startup log should now say
   "Google sign-in enabled" instead of "disabled". Visit
   http://localhost:3000, and the header's "Sign in with Google" button
   should take you through Google's consent screen and back signed in.

For a real (non-localhost) deployment: add that deployment's actual
callback URL as another authorized redirect URI in step 4 (Google
supports multiple), set `GOOGLE_REDIRECT_URL`/`FRONTEND_BASE_URL` to the
real URLs, set `COOKIE_SECURE=true` (the session/state cookies won't be
stored by the browser at all over plain HTTP otherwise), and move the
consent screen from Testing to **Published** (in the OAuth consent
screen settings) so it isn't limited to a fixed test-user list — Google
may require a verification review for a published app requesting more
than basic profile/email scopes, though the profile+email scopes this
app requests are unlikely to trigger that.

### Access control (who's actually allowed to use the app)

Signing in with Google is authentication, not authorization — a valid
session alone doesn't get you past `api.RequireApproved` on every real
route (the BCP proxy, My Events, Stats, follows/recent-events sync).
Every new sign-in starts `role=user, status=pending` (migration
`0007_user_access_control.sql`) and can't use anything beyond signing
in itself until an admin approves them via the admin API
(`GET/POST /api/admin/users...`, `internal/api/admin.go`) — see
`app/lib/auth.ts`'s `CurrentUser.status` in the frontend for how a
pending/rejected account's own experience is gated.

Set **`ADMIN_EMAILS`** (comma-separated, case-insensitive — see
`.env.example`) to your own email before relying on this anywhere real:
every sign-in matching it is auto-promoted to `role=admin,
status=approved`, every time it signs in, not just the first — this is
the only way to get an initial admin without a manual database
update, and without it nobody (including you) can ever approve anyone.

One deliberate exception: `GET /api/events/:id/players` only requires a
valid session, not approval, since the frontend's BCP-profile-linking
roster picker (`/welcome`) needs it to work for a still-pending account
— see `BCPHandler.Register`'s doc comment in `internal/api/bcp.go`.

## Running the whole stack with Docker

This repo also has a `docker-compose.yml` that brings up Postgres, this
backend, and the frontend together — three containers, one command. It
assumes this repo sits next to `../brass-ledger-web` on disk, which
is the layout both repos are already in.

```bash
./run.sh
```

That builds all three images and starts them, with sensible dev-only
defaults for the Postgres credentials (override them with a local `.env`
file in this directory if you want different ones — see the comment at
the top of `docker-compose.yml`). Google sign-in vars
(`GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REDIRECT_URL`,
`FRONTEND_BASE_URL`, `COOKIE_SECURE`) are passed through the same way —
put them in that local `.env` file too, and see "Google sign-in setup"
above. Once it's up:

- Frontend: http://localhost:3000
- Backend: http://localhost:8080/healthz
- Postgres: localhost:5432 (reachable directly too — the dev-only
  defaults are user/db/password `brassledger`/`brass_ledger`/
  `brassledger`, or just `make db-shell` for a `psql` shell without
  needing to remember them)

`./run.sh down` stops everything (add `-v` to also delete the Postgres
data volume, if you want a clean slate). `./run.sh -d` runs it detached.

Already have the stack running and just changed some code? `make
docker-reset` rebuilds the images from your latest changes and restarts
everything (detached), without needing to think about which flags
`run.sh`/`docker compose` want for that. Then `make smoke` for a fast
"did it actually come up" check (healthz/readyz/auth routing/frontend —
no BCP calls) before digging in further by hand.

One prerequisite: the backend's Docker build copies `go.sum`, so run `go
mod tidy` locally at least once (see "Running locally" above) before the
first `./run.sh` — after that, Docker handles everything itself.

## Deploying to Cloudflare

This backend deploys as a [Cloudflare Container](https://developers.cloudflare.com/containers/)
(runs the existing `Dockerfile` unchanged) fronted by a small Worker
(`src/worker.ts`) — see `wrangler.jsonc`. The sibling frontend deploys
separately to Cloudflare Workers via `vinext` (see its own README/CLAUDE.md).
Requires the $5/mo Workers Paid plan (Containers aren't available on the
free plan) and a managed Postgres (e.g. [Neon](https://neon.tech), which
has a free tier — this repo was never meant to run its own Postgres
server in production, see `internal/config`'s `DatabaseURL` comment).

One-time setup:

```sh
npm install                      # installs wrangler + the Worker's deps
npx wrangler login                # authenticate the CLI once

# Secrets — never committed, set directly on the Worker:
npx wrangler secret put DATABASE_URL          # Neon's connection string (sslmode=require)
npx wrangler secret put GOOGLE_CLIENT_ID
npx wrangler secret put GOOGLE_CLIENT_SECRET
```

Then, and on every subsequent deploy:

```sh
npx wrangler deploy
```

`wrangler.jsonc`'s `vars` already set `FRONTEND_BASE_URL`,
`GOOGLE_REDIRECT_URL`, `COOKIE_SECURE`, and `PORT` for the production
domain — update those there (not as secrets) if the domain ever changes.
The Google Cloud Console OAuth client's authorized redirect URI must
include `GOOGLE_REDIRECT_URL`'s value exactly (see "Google sign-in
setup" above). The `routes` entry provisions DNS + TLS for
`api.brass-ledger.app` automatically on first deploy, since the zone is
already on Cloudflare — no manual DNS record needed.

## Logging

Every request (method, path, status, latency, user agent) and every
`log.Printf`/`log.Fatalf` call this service makes — startup, database
migrations, and each step of the Google sign-in flow (state mismatches,
token exchange failures, who actually signed in) — goes to **both**
stdout and a log file, so the full history is still there to read back
after the fact instead of only existing in whatever terminal happened to
be open at the time. Controlled by `LOG_FILE` in `.env` (default
`logs/backend.log`; set it to an empty string for stdout-only).

```bash
make logs        # print the log file so far
make logs-tail    # follow it live (Ctrl-C to stop)
```

Running via Docker Compose (`./run.sh`)? The backend container writes to
`/logs/backend.log` inside itself, bind-mounted to this repo's own
`./logs/` directory on the host — so `make logs`/`make logs-tail` (or
just `cat`/`tail` on `logs/backend.log` directly) see exactly what the
containerized server wrote, no `docker compose exec`/`docker cp`
needed. `run.sh` creates that directory for you (world-writable, since
the backend's container runs as a non-root user) the first time you
start the stack.

Debugging a sign-in that isn't working? The access log line for the
`/auth/google/callback` request (status, latency) sits right next to the
`log.Printf` line explaining *why* it failed (missing/mismatched state,
Google rejecting the token exchange, a database error creating the
session) — `grep google logs/backend.log` after a failed attempt
usually goes straight to the answer.

The frontend has its own equivalent log file for what the *browser*
sees (failed `/api/me` checks, sign-out errors, uncaught exceptions) —
see its README's "Logging" section; the two files are usually worth
reading together when a sign-in fails, since the error might only show
up on one side.

## Testing

Table-driven tests cover `internal/bcp` (the cache and BCP client, the
latter against a stub HTTP server rather than the real BCP API),
`internal/config`, and `internal/auth` (the hand-rolled Google OAuth
client, against a stub HTTP server standing in for Google's token/userinfo
endpoints). `internal/api`'s route tests (both the BCP proxy and the
auth routes) need Echo, so — like the rest of this service — they need
`go mod tidy` to have run at least once.

```bash
go test ./...          # everything
go test ./... -v       # with per-case output
go test ./... -race    # cache.go's concurrency guarantees are worth
                        # checking under the race detector specifically
```

`internal/user` (the Postgres-backed user/session store) needs a real
Postgres to say anything a mock wouldn't just assert back at itself —
it has its own integration test suite
(`internal/user/store_integration_test.go`), gated behind the
`integration` build tag so it never runs as part of plain
`go test ./...` and never requires a database just to work on anything
else:

```bash
make test-integration   # against the local docker-compose Postgres (must be up — `make docker-up`)

# equivalent, if you'd rather run it directly:
DATABASE_URL=postgres://brassledger:brassledger@localhost:5432/brass_ledger?sslmode=disable \
  go test -tags=integration ./internal/user/...
```

CI runs this too (see `.github/workflows/ci.yml`'s `integration` job,
against a Postgres service container) and it gates every deploy, same
as the regular `test` job. `internal/db` (a thin wrapper over
`pgxpool`, plus the migration runner) is exercised implicitly by this
same suite's `db.Migrate` call in its test setup, rather than having
tests of its own.

## Project layout

```
cmd/server/       entry point — wires config, the DB pool, BCP client, auth, and routes together
internal/config/  environment-variable configuration
internal/db/      Postgres connection setup (pgx) and the migration runner
internal/bcp/     BCP API client, with server-side caching/rate-limiting
internal/auth/    hand-rolled Google OAuth2 client (no third-party OAuth/JWT libraries)
internal/user/    Postgres-backed user + session store
internal/api/     HTTP route handlers (BCP proxy + Google sign-in/session routes)
```

As more real features land (follows, notes), they'll get their own
`internal/` packages rather than piling everything into `cmd/server`.

## TODO

- [x] BCP proxy/cache — event info, rosters, pairings, placings, and ITC
  rankings are now fetched and cached here (`internal/bcp`,
  `internal/api/bcp.go`) instead of directly from the frontend. See
  `internal/bcp/types.go`'s doc comment for the scope limit this
  inherited from the frontend.
- [x] Unit tests — table-driven tests for `internal/bcp`,
  `internal/config`, and `internal/auth`. See "Testing" above.
- [x] User accounts (Google sign-in) — `internal/auth` (OAuth client),
  `internal/user` (Postgres store), routes in `internal/api/auth.go`.
  See "Google sign-in setup" above. Cross-device following/notes (the
  features this unblocks) are still TODO on the frontend side.
- [x] Database schema/migrations — `internal/db/migrate.go` applies
  `internal/db/migrations/*.sql` automatically on startup. Currently
  just `users` + `sessions`; per-event follows/notes tables will be
  added the same way once those features land.
- [x] Auth/sessions — opaque server-side session tokens in Postgres
  (`internal/user`), set as an httpOnly cookie. Chosen over JWTs
  specifically so a session can be revoked (sign-out actually deletes
  it) rather than remaining valid until its own expiry.
- [x] Containerization — Dockerfile here and in the frontend repo, plus
  `docker-compose.yml`/`run.sh` here to run the whole stack (Postgres +
  backend + frontend) with one command. See "Running the whole stack
  with Docker" above.
- [x] Logging — every request and every application-level log line
  (startup, migrations, Google sign-in flow) now goes to a log file
  (`LOG_FILE`, default `logs/backend.log`) in addition to stdout, with
  `make logs`/`make logs-tail` to read it back. See "Logging" above.
- [ ] Deployment (Fly.io/Railway alongside wherever the frontend ends up
  — see the frontend repo's own deployment TODO). The Dockerfiles above
  are what a real deploy would build from either way. Remember to add
  the real deployment's callback URL in Google Cloud Console and flip
  `COOKIE_SECURE=true` when this happens (see "Google sign-in setup").
- [ ] CI (build + `go vet`/`golangci-lint` on push)
- [x] Tighten CORS — now scoped to `FRONTEND_BASE_URL` with
  `AllowCredentials: true` (required for the session cookie to reach
  this API cross-origin at all), instead of the previous wide-open
  `middleware.CORS()`.
- [x] "My events" (past/present/future) — a signed-in account can link a
  Best Coast Pairings profile (`bcp_user_id` on `users`,
  `POST /api/me/bcp-profile`) and fetch its full BCP tournament history,
  classified into Past/Present/Future (`GET /api/me/events`,
  `internal/api/me.go`). See `internal/bcp/history.go`'s "Per-user event
  history" section for the two BCP endpoints this combines
  (`FetchPlayerEventHistory`/`FetchPlacingHistory`) and why a plain
  registration list alone can't tell past from present from future.
