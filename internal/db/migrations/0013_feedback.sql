-- Persists every bug report/suggestion submitted through the frontend's
-- floating feedback widget (see internal/api/feedback.go) — previously
-- email-alert-only (internal/notify's NotifyFeedback), so there was no
-- way to see what's outstanding except an inbox. submitted_by_user_id is
-- nullable since submitting needs no session at all; the name/email are
-- captured at submission time rather than joined against users live, so
-- a report still reads correctly even if that account is later renamed
-- or deleted.
CREATE TABLE IF NOT EXISTS feedback (
    id                   BIGSERIAL PRIMARY KEY,
    kind                 TEXT NOT NULL CHECK (kind IN ('bug', 'suggestion')),
    message              TEXT NOT NULL,
    page                 TEXT NOT NULL DEFAULT '',
    contact_email        TEXT NOT NULL DEFAULT '',
    submitted_by_user_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
    submitted_by_name    TEXT NOT NULL DEFAULT '',
    submitted_by_email   TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Speeds up the admin panel's open-count badge and its "open first"
-- ordering — cheap either way at this project's scale, but no reason not
-- to have it (same reasoning as migration 0007's users_status_idx).
CREATE INDEX IF NOT EXISTS feedback_status_idx ON feedback (status);
