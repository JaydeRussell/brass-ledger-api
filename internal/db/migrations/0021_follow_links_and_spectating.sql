-- A follow link lets anyone holding its token view one event from the
-- point of view of the player who created it. player_id is that player's
-- BCP roster entry id in the event. The token is a random value, not a
-- credential for the account: it only unlocks already-public BCP data for
-- this one event, and the owner can copy it again at any time.
CREATE TABLE IF NOT EXISTS follow_links (
    id          BIGSERIAL PRIMARY KEY,
    token       TEXT NOT NULL UNIQUE,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id    TEXT NOT NULL,
    player_id   TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, event_id)
);

-- One spectated event per account, from a follow link (follow_link_id set)
-- or from picking a player after a search (follow_link_id null).
-- Deleting the link removes every row that came from it.
CREATE TABLE IF NOT EXISTS spectating (
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id        TEXT NOT NULL,
    player_id       TEXT NOT NULL,
    follow_link_id  BIGINT REFERENCES follow_links(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_id)
);

CREATE INDEX IF NOT EXISTS follow_links_expires_at_idx ON follow_links (expires_at);
CREATE INDEX IF NOT EXISTS spectating_expires_at_idx ON spectating (expires_at);
