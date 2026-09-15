---
name: run-locally
description: Launch and verify Brass Ledger (this backend, brass-ledger-api, together with the sibling brass-ledger-web frontend) against the local Docker Compose stack — rebuild, confirm the containers are current, then drive the frontend with the Chrome browser tools. Use this whenever asked to run, start, screenshot, or click-through-verify a change in the real app.
---

# Running Brass Ledger locally

Brass Ledger is two repos — this backend (`brass-ledger-api`) and the
sibling frontend (`brass-ledger-web`) — that share one Docker Compose
stack, driven from this repo's `run.sh`. There is no working `go run
./cmd/server`-against-a-real-frontend path for real click-through
testing in this environment; always go through Docker.

## 1. Rebuild before you test

**`localhost:3000`/`:8080` are NOT source-mounted.** The containers
serve a snapshot built at image-build time; only `logs/` is
bind-mounted. Editing files on disk (in either repo) does nothing to
the running app until you rebuild:

```bash
./run.sh -d --build
```

Takes ~30s, rebuilds and restarts both `brass-ledger-frontend-1` and
`brass-ledger-backend-1` (leaves `brass-ledger-db-1` running as-is).
Run it from this repo even if only the frontend changed — it rebuilds
whichever images are stale.

**Don't skip this.** A "the feature I just wrote isn't showing up" or
"still loading" symptom during live testing is almost always a stale
image, not a real bug — confirmed the hard way debugging the frontend's
mission-matchup panel on 2026-09-12/13. If you're not sure whether a
rebuild is needed, compare container age to your latest edit before
concluding a live-test failure is a real bug:

```bash
docker ps --format '{{.Names}}\t{{.CreatedAt}}'
stat -f "%Sm %N" path/to/file/you/just/edited.go
```

If the container predates the edit, rebuild first.

## 2. Confirm it's up

```bash
docker ps
```

Expect `brass-ledger-frontend-1`, `brass-ledger-backend-1`, and
`brass-ledger-db-1` all `Up`/healthy.

## 3. Drive it with the Chrome browser tools

The UI to click through lives in the frontend at `http://localhost:3000`
(this backend has no UI of its own). Load the browser tools if they're
deferred:

```
ToolSearch("select:mcp__claude-in-chrome__tabs_context_mcp,mcp__claude-in-chrome__navigate,mcp__claude-in-chrome__computer,mcp__claude-in-chrome__read_page,mcp__claude-in-chrome__tabs_create_mcp,mcp__claude-in-chrome__tabs_close_mcp,mcp__claude-in-chrome__read_console_messages,mcp__claude-in-chrome__read_network_requests")
```

- Screenshot before and after the interaction you're verifying — don't
  just confirm a request 200s, actually click through the flow the way
  a user would.
- Call `read_network_requests`/`read_console_messages` (with a pattern)
  *before* triggering the action you want to observe — both only track
  from the moment they're first called on a tab, so calling them
  afterward misses everything that already happened.
- A signed-in Google account, already linked to a real BCP profile, is
  usable in this stack — most real routes (`/api/me/*`, sync, stats)
  need it to be meaningfully exercised.
- Close any tab you opened once you're done (`tabs_close_mcp`).

## 4. Read the logs if something's off

The containers are distroless — no shell, so `docker exec ... sh` (or
`bash`/`cat`/`tail`) fails with "executable file not found in $PATH".
Read the bind-mounted log files directly from the host instead:

```bash
tail -30 logs/backend.log
tail -30 ../brass-ledger-web/logs/frontend.log
```

Backend access-log lines are one JSON object per request (method, uri,
status, latency, user agent) — grep by path or status to confirm a
specific request actually happened the way you expect, rather than
guessing from the UI alone.
