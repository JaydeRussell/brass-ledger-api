-- Private per-round notes (e.g. matchup prep, list reminders) — same
-- cross-device-sync reasoning as migration 0003's user_follows/
-- user_recent_events, but new state (never lived in localStorage first).
-- One row per (user, event, round); an empty note is deleted rather than
-- stored (see internal/user/store.go's SetRoundNote), so this table only
-- ever holds real notes, not a row per round someone merely glanced at.
CREATE TABLE IF NOT EXISTS user_round_notes (
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id   TEXT NOT NULL,
    round      INTEGER NOT NULL,
    note       TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_id, round)
);
