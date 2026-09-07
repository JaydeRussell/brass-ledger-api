# Project rules

## Scope limit — no pairing logic, ever

This app (and its sibling frontend, `brass-ledger-web`) only ever
displays data Best Coast Pairings (BCP) has already published — rosters,
event info, already-decided pairings, already-computed placings.
**Never add anything that computes, ranks, or suggests a pairing or
matchup.** Challengers Cup's event pack bans "AI programs, algorithms, or
methodology... for the pairings process" — that's broader than just AI —
so this holds even for a plain deterministic heuristic with no AI
involved. See the doc comment at the top of `internal/bcp/types.go`
before touching anything in that area.

## Be respectful of BCP's API

`internal/bcp` talks to BCP's undocumented, unofficial data API — no
partnership or rate-limit agreement, just an endpoint their own web app
happens to expose. It caches server-side and enforces a minimum refetch
interval so every user of this app shares one rate limit against BCP
instead of each browser hammering it independently. Don't add polling,
aggressive retries, or fetch more than a feature actually needs — see the
frontend's `CLAUDE.md` for the full list this inherited from (the
frontend used to talk to BCP directly; that logic moved here).

---

# Current status (as of 2026-09-07) — read this first in a new session

This section is a snapshot for picking the project back up, not a
permanent rule — feel free to rewrite/replace it entirely once it's
stale, rather than appending to it forever.

## What's built and working

- **BCP proxy/cache** (`internal/bcp`, routed in `internal/api/bcp.go`) —
  done, tested, stable. Not touched recently.
- **Google sign-in** — a complete, hand-rolled OAuth2 flow with no
  third-party OAuth/JWT libraries:
  - `internal/auth` — the Google OAuth client (`google.go`), fully unit
    tested against a stub HTTP server.
  - `internal/user` — Postgres-backed user + session store (opaque
    server-side session tokens, not JWTs, specifically so a session can
    be revoked on sign-out rather than remaining valid until its own
    expiry). Not unit tested — needs a real Postgres to test meaningfully.
  - `internal/api/auth.go` — the `/auth/google/login`,
    `/auth/google/callback`, `/auth/logout`, `/api/me` routes. Tested
    against an in-memory fake store (`internal/api/auth_test.go`).
  - `internal/db/migrate.go` — applies `internal/db/migrations/*.sql`
    (currently just `users` + `sessions`) automatically on startup.
  - See the README's "Google sign-in setup" for the full Google Cloud
    Console walkthrough — the user has already created an OAuth client
    and its real Client ID/Secret are in `.env` (gitignored correctly;
    **do not** put real credentials in `.env.example` — that file is
    deliberately git-tracked as a template, see the `!.env.example` line
    in `.gitignore`. This actually happened once already this project —
    caught and fixed before anything was committed.).
- **Logging** — every request and every application-level log line
  (startup, migrations, each step of the Google sign-in flow) goes to
  both stdout and `logs/backend.log` (`LOG_FILE` in `.env`). See the
  README's "Logging" section. `internal/applog` has its own test suite.
- **"My events" (past/present/future)** — a signed-in account can link a
  Best Coast Pairings profile and see its full BCP tournament history,
  classified into Past/Present/Future:
  - `internal/db/migrations/0002_bcp_profile.sql` — adds `bcp_user_id`
    (nullable, unindexed) to `users`.
  - `internal/user/store.go` — `User.BcpUserID` field, `SetBcpUserID`.
  - `internal/bcp/types.go`/`client.go` — two new cached, paginated
    fetch methods: `FetchPlayerEventHistory` (every event ever
    registered for — `/v1/players?userId=...`, no dates) and
    `FetchPlacingHistory` (already-concluded events with dates/placing —
    `/v1/eventplacings?userId=...`). See client.go's "Per-user event
    history" section comment for the full design rationale — worth
    reading before touching this, since it encodes real API shapes
    discovered via live browser research (BCP's API is undocumented).
  - `internal/api/me.go` — `POST /api/me/bcp-profile` (link/unlink) and
    `GET /api/me/events` (fetches both BCP endpoints above, plus a
    `FetchEventInfo` fallback call for any registered-but-not-yet-placed
    event, to tell Present from Future via `Started`/`Ended`).
  - Full test coverage: `internal/bcp/client_test.go` (pagination,
    edge cases) and `internal/api/me_test.go` (auth gating, linking,
    classification) — all passing via `go test`.
  - **Confirmed working end-to-end against a real Postgres and a real
    BCP account** — linked a real profile, saw real Present/Future/Past
    results in the browser. One real bug turned up along the way and is
    now fixed (see below).
  - **`decodeNextKey` bug fix**: BCP's `/v1/eventplacings` pagination
    cursor (`nextKey`) is inconsistent — most pages return it
    pre-encoded as a JSON string (base64 of the underlying cursor
    object, used as-is for the next page's `nextKey` query param), but
    at least one real response instead returned the raw cursor object
    itself, unencoded (confirmed via `logs/frontend.log`'s error
    message, not a hypothetical). `bcpPlayersByUserResponse.NextKey` and
    `bcpPlacingHistoryResponse.NextKey` are now `json.RawMessage`
    instead of `string`, decoded by the new `decodeNextKey` helper
    (base64-encodes anything that isn't already a JSON string). Has its
    own unit test (`TestDecodeNextKey`) plus a regression case in
    `TestFetchPlacingHistory` reproducing the exact failing response.
    Worth knowing about if pagination on either history endpoint ever
    errors again with a JSON-decode message.
  - **Linking UX**: the frontend's default flow for finding your own BCP
    id is no longer "dig through the browser's Network tab" (that was
    the only option briefly, and it was rough) — it's now picking
    yourself out of an event roster you already know you played in,
    since `/api/events/:id/players` already returns every player's
    `bcpUserId` (this backend never had to change for that — the field
    was always there, just not surfaced in the frontend UI). See the
    frontend's `CLAUDE.md`/`myEventsPanel.tsx`'s `RosterPicker` for the
    implementation; pasting a raw id/URL is kept as a manual fallback.
  - Frontend: `app/lib/myEvents.ts` +
    `app/components/auth/myEventsPanel.tsx` in `brass-ledger-web`,
    rendered inside `AuthStatus`'s account dropdown (the user's explicit
    placement choice — no dedicated tab). Note: the frontend has since
    moved this onto its own `/my-events` route and added a left-hand nav
    drawer — see that repo's `CLAUDE.md` for the current layout; nothing
    here changed for that move.
- **Cross-device sync for follows + recent events** — the two pieces of
  state that used to live only in the frontend's per-browser
  `localStorage` (see the frontend's `app/page.tsx`/`app/lib/
  recentEvents.ts`) now persist to a signed-in account, following it
  across devices — the actual point of having accounts, and the last
  item on the old TODO list below:
  - `internal/db/migrations/0003_follows_recent_events.sql` — two new
    tables, both scoped by `user_id`: `user_follows` (one row per
    followed team/player within one event — composite primary key
    `user_id, event_id, kind, ref_id`) and `user_recent_events` (one row
    per event a user has viewed, primary key `user_id, event_id`, with a
    `(user_id, last_viewed_at DESC)` index for the common "most recent
    first" query).
  - `internal/user/store.go` — `Follow`/`RecentEvent` types and
    `MaxRecentEvents = 8` (mirrors the frontend's own trim limit), plus
    `ListFollows`/`AddFollow` (upsert, idempotent)/`RemoveFollow` and
    `ListRecentEvents`/`RecordRecentEvent` (upserts, bumps
    `last_viewed_at`, then trims anything past `MaxRecentEvents` in the
    same call so the cap holds regardless of which device wrote most
    recently).
  - `internal/api/sync.go` — `RegisterSyncRoutes`, all session-gated the
    same way `me.go`'s routes are (`requireUser` factors out the
    cookie-then-store-lookup check both files' routes need):
    `GET`/`POST /api/me/events/:eventId/follows`,
    `DELETE /api/me/events/:eventId/follows/:kind/:refId`, and
    `GET`/`POST /api/me/recent-events`. Deliberately action-shaped (add
    one follow, remove one follow) rather than "replace the whole list"
    — see the doc comment on `RegisterSyncRoutes` for why a
    replace-the-whole-array approach is a bad fit for a server multiple
    devices can hit concurrently.
  - `internal/api/auth.go`'s `userStore` interface grew the five new
    methods above (same pattern as `SetBcpUserID` before it).
  - Full test coverage: `internal/api/sync_test.go` (auth gating, add/
    list/remove idempotency, per-event isolation, recent-events ordering
    and validation) against `fakeUserStore` (extended in
    `internal/api/auth_test.go` with in-memory follows/recent-events
    maps) — passing via `go test` on the stdlib-only subset this
    environment can actually run (see "Environment quirks" below); not
    yet exercised against a real Postgres or clicked through live.
  - Frontend: `app/lib/follows.ts` (new) + `app/lib/recentEvents.ts`
    (extended, not replaced — the original localStorage-only functions
    are still there and still used for a signed-out/guest visitor) +
    `app/page.tsx` (branches on `useCurrentUser()`'s signed-in state to
    decide which backing store following/recent-events use). See the
    frontend's `CLAUDE.md` for the fuller writeup.
- **Stale-event reclassification for "My events"** — some organizers
  never flip BCP's own "ended" switch even once an event is clearly over,
  which used to leave it stuck in the "Ongoing"/Present bucket forever.
  `internal/api/me.go` now has `isStaleEvent(endDate string) bool` /
  `staleEventAfter` (currently 3 days) / `parseBCPDate` (tries both date
  shapes BCP actually sends — see its doc comment): a registered-but-not-
  yet-placed event that's `Started && !Ended` but whose listed end date
  is more than `staleEventAfter` in the past is now classified into Past
  instead of Present. Doesn't touch the placings-history-backed Past
  entries (those are already concluded by definition) or Future. Table-
  driven unit test `TestIsStaleEvent` plus a new `evt-stale` case in
  `TestMyEvents_ClassifiesPastPresentFuture`.

## What's NOT yet done / verified

- **Fixed 2026-09-07**: `internal/api/bcp_test.go` and `internal/api/auth_test.go`
  both independently declared a `newTestEcho`/`doRequest` helper pair with
  different signatures — a same-package redeclaration that would fail to
  compile. Renamed `bcp_test.go`'s pair to `newBCPTestEcho`/`doBCPRequest`
  (matching `newMeTestEcho`/`newSyncTestEcho`'s per-feature naming in the
  other test files here) rather than touching `auth_test.go`, which many
  more tests already depend on. This slipped through because at the time
  `internal/api` couldn't actually be built/tested in the old cloud
  sandbox (needs echo/pgx), so a same-package name collision like this had
  no way to surface there; only `gofmt -l` (which just parses each file
  independently, so it's blind to cross-file redeclarations) had been run
  against it. **Since resolved**: now that work happens in a real terminal
  on the user's own machine (see "Environment quirks" below), `go build
  ./...`, `go vet ./...`, and `go test ./...` all run for real and all
  pass tree-wide, including `internal/api` — this category of bug now
  surfaces immediately.
- ~~Nobody has actually run the sign-in flow end-to-end yet~~ —
  **confirmed working 2026-09-07.** The user ran `./run.sh` and signed
  in for real; `logs/backend.log` shows the full
  `/auth/google/login` → Google callback (matching state) →
  `google callback: signed in user 1 (...)` → `/api/me` 200 sequence,
  and `brass-ledger-web`'s `logs/frontend.log` shows the client's
  auth check flipping from `signedIn: false` to
  `signedIn: true, userId: 1` at the same moment — the two logs
  corroborating each other end-to-end, which is exactly what the
  logging work was for. If something regresses later, that's the shape
  of evidence to look for again.
- `DATABASE_URL` in `.env` is still the placeholder value. That's fine
  *only* for the Docker Compose path (`./run.sh`) — `docker-compose.yml`
  hardcodes the backend's real `DATABASE_URL` from
  `POSTGRES_USER`/`POSTGRES_PASSWORD`/`POSTGRES_DB` and ignores whatever
  `.env` says. It would need a real value for `go run ./cmd/server`
  outside Docker.
- ~~Cross-device follows/notes (the actual feature accounts unlock) — not
  started.~~ Follows + recent events are now synced (see above); a
  "notes" feature was never actually specced beyond that TODO-list
  mention, so there's nothing further planned there unless the user
  raises it again.
- The new sync routes/migration are verified only via `go test` against
  an in-memory fake store and `npx tsc`/`npm run lint`/`npm test` on the
  frontend side — nobody has run this against a real Postgres or clicked
  through a real follow/unfollow or event-revisit in the browser yet.
  Worth a real pass before trusting it the way "my events" now is.
- No CI yet.

## Environment quirks that will trip up a new session

**As of 2026-09-07, work happens directly in a terminal on the user's own
Mac** — this Claude Code session's `Bash` tool runs natively in
`brass-ledger-api` (darwin/arm64), with `brass-ledger-web` as a
true sibling directory at the same level, no bridge/device tools
involved. The remote-devices bridge + separate cloud-sandbox split
described in earlier sessions (two different filesystems, `device_bash`
vs `Bash`, `device_commit_files` to sync changes across) **no longer
applies** — ignore any instinct to route around a missing toolchain or
copy files between environments.

**The frontend is in scope from this repo too.** The user has said to
treat `brass-ledger-web` (sibling directory, `cd
../brass-ledger-web`) as part of the same working session going
forward, not a separate repo to be handed off to a different context —
read/edit/build/test it directly here when a task touches it, same as
any package in this repo. It's still its own git repo with its own
`CLAUDE.md` (read that before editing anything under it), just no longer
a hard context boundary.

Confirmed directly in this terminal: `go`, `docker`, `node`, and `npm`
are all on `PATH`. `go build ./...`, `go vet ./...`, and `go test ./...`
all pass tree-wide in this repo, including `internal/db`, `internal/user`,
`internal/api`, and `cmd/server` (the pgx/echo-dependent packages the old
cloud sandbox couldn't reach the module proxy for) — no more reason to
hedge build/test claims for those packages. `npm run build` in
`brass-ledger-web` also passes cleanly (Next.js 16 + Turbopack,
confirmed 2026-09-07) — the old "SWC binary for linux/arm64" failure was
specific to the previous sandboxed Linux VM and does not reproduce on
this native darwin/arm64 terminal.

Still true regardless of environment: `DATABASE_URL` in `.env` is a
placeholder that's only fine for the `./run.sh` Docker Compose path (see
"What's NOT yet done" above) — `go run ./cmd/server` outside Docker needs
a real value.
