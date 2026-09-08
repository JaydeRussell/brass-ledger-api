-- A per-account unguessable token used to authenticate the subscribable
-- calendar feed (internal/api's GET /api/calendar/:token) — a calendar
-- app polls this URL on its own schedule with no session cookie, so the
-- token in the URL itself is the credential, the same way Google/iCloud's
-- own private calendar-subscription links work. Nullable: generated lazily
-- on first request (see user.Store.EnsureCalendarToken), not at sign-up.
ALTER TABLE users ADD COLUMN IF NOT EXISTS calendar_token TEXT;

-- Partial (skips NULLs, i.e. accounts that have never requested a
-- calendar link) unique index — this is the column GetUserByCalendarToken
-- looks up by, so it needs to be indexed, and uniqueness is what makes
-- the token a safe capability credential in the first place.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_calendar_token
    ON users (calendar_token) WHERE calendar_token IS NOT NULL;
