# Brass Ledger

[![CI/CD](https://github.com/JaydeRussell/brass-ledger-api/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/JaydeRussell/brass-ledger-api/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/JaydeRussell/brass-ledger-api/badges/coverage.json)](https://github.com/JaydeRussell/brass-ledger-api/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/JaydeRussell/brass-ledger-api)](LICENSE)

**Tournament Companion** for Warhammer 40k — this is the backend for
[brass-ledger-web](../brass-ledger-web). Written in Go, using
[Echo](https://echo.labstack.com/) and Postgres.

This service proxies and caches Best Coast Pairings' (BCP) data — event
info, rosters, pairings, placings, ITC rankings (`internal/bcp`, routed
in `internal/api/bcp.go`) — so every user of this app shares one
server-side cache and rate limit against BCP instead of each browser
hitting it independently. It also handles Google sign-in with
server-side sessions (`internal/auth`, `internal/user`), and the
account-backed features that build on that (cross-device follows,
recent events, player stats).

Same scope limit as the frontend: this only ever retrieves data BCP
already publishes. It never computes, ranks, or suggests a pairing/matchup
of any kind — see the doc comment at the top of `internal/bcp/types.go`.

## Requirements

- Go 1.27+ (see `go.mod`)
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

Database migrations (`internal/db/migrate.go`) apply automatically on
startup — there's no separate migrate command to run.

Google sign-in is optional — leave `GOOGLE_CLIENT_ID`/
`GOOGLE_CLIENT_SECRET` unset in `.env` and everything else (the BCP
proxy, health checks) still works. To enable it, create a Web
application OAuth client in [Google Cloud Console](https://console.cloud.google.com/)
with redirect URI `http://localhost:8080/auth/google/callback`, then set
`GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` in `.env`. Also set
`ADMIN_EMAILS` (comma-separated, your own email) — every sign-in
starts pending until an admin approves it, and this is the only way to
bootstrap the first admin account. See `.env.example` for every
variable's full description.

## Running the whole stack with Docker

`docker-compose.yml`/`run.sh` here bring up Postgres, this backend, and
the frontend together — three containers, one command. Assumes this
repo sits next to `../brass-ledger-web` on disk.

```bash
./run.sh          # builds and starts everything
./run.sh down     # stops everything (add -v to also drop the Postgres volume)
```

Once it's up: frontend at http://localhost:3000, backend at
http://localhost:8080/healthz, Postgres at localhost:5432 (or `make
db-shell` for a `psql` shell without needing to remember the dev-only
credentials). Google sign-in vars go in a local `.env` file in this
directory, same as above.

One prerequisite: the backend's Docker build copies `go.sum`, so run `go
mod tidy` locally at least once before the first `./run.sh`.
