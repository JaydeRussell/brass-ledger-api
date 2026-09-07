-- Cross-device sync for the two pieces of per-browser localStorage state
-- the frontend previously kept entirely client-side (see
-- brass-ledger-web's app/page.tsx `following`/`followingKey` and
-- app/lib/recentEvents.ts): which teams/players a signed-in user follows
-- within a given event, and which events they've recently viewed. Both
-- are scoped to a user_id, mirroring bcp_user_id's manual-link model —
-- nothing here is computed or ranked, it's just persistence for choices
-- the user already made client-side (see CLAUDE.md's "no pairing logic"
-- scope rule, which doesn't apply here since nothing is being decided).

-- One row per followed team/player within one event. kind+ref_id mirrors
-- the frontend's Followed union (`{kind: "team", teamPlayerId}` or
-- `{kind: "player", playerId}`) — ref_id is whichever one applies. label
-- is stored redundantly (rather than re-fetched from BCP on every read)
-- so a follow still displays sensibly even if BCP's roster data for that
-- event later changes shape or becomes unavailable.
CREATE TABLE IF NOT EXISTS user_follows (
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id   TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('team', 'player')),
    ref_id     TEXT NOT NULL,
    label      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_id, kind, ref_id)
);

-- One row per event a user has viewed, replacing the frontend's
-- client-side MAX_RECENT_EVENTS=8 trimming (see recentEvents.ts) with the
-- same trim applied server-side on write (internal/user/store.go's
-- RecordRecentEvent) — no separate cleanup job needed.
CREATE TABLE IF NOT EXISTS user_recent_events (
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id       TEXT NOT NULL,
    event_name     TEXT NOT NULL,
    team_event     BOOLEAN NOT NULL,
    last_viewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_id)
);

CREATE INDEX IF NOT EXISTS user_recent_events_user_id_last_viewed_at_idx
    ON user_recent_events (user_id, last_viewed_at DESC);
