-- Renumbered from 0014_ to 0015_ (it originally collided with
-- 0014_dossier_public.sql, which shipped from a separate branch).
--
-- Safe to renumber precisely because every statement below is
-- IF NOT EXISTS-guarded: Migrate keys schema_migrations on the file
-- path, so the rename makes this run once more against every existing
-- database, where it does nothing at all. Its old version row stays in
-- schema_migrations, matching no file — which Migrate ignores.
--
-- 0014_dossier_public.sql was deliberately left alone: it's a bare
-- ALTER TABLE ... ADD COLUMN with no IF NOT EXISTS, so renaming *it*
-- would re-run it, error, and take startup down on every deploy.
--
-- Mutual "friending" between two accounts — see internal/api/friends.go.
-- Deliberately requires acceptance both ways (a request must be
-- accepted before either side can see the other's data) rather than a
-- one-directional follow like migration 0003's user_follows: a friend's
-- upcoming events (their real-world schedule) is more sensitive than a
-- per-event team/player follow, so this should read like the mutual
-- trust "friending" implies, not a public follow count.
--
-- One row per (requester, recipient) *ordered* pair — requester_id and
-- recipient_id are never swapped once a row exists, even after it's
-- accepted, so "who sent this" stays answerable. A friendship is
-- symmetric in practice (once accepted, either side can see the
-- other's linked BCP events — see FriendsHandler.Events), but the row
-- itself keeps its original direction.
--
-- The partial unique index (rather than a plain UNIQUE(requester_id,
-- recipient_id)) exists so a *declined* request doesn't permanently
-- block a future one between the same two people — only a pending or
-- already-accepted row blocks a duplicate; a declined one is inert
-- history. Both directions are still distinct rows (A→B and B→A can
-- both exist mid-flight if two people send requests to each other at
-- the same time) — AcceptFriendRequest resolves that by accepting
-- whichever one is acted on and leaving the other's fate to whoever
-- looks at it next (see that method's own doc comment).
CREATE TABLE IF NOT EXISTS friend_requests (
    id            BIGSERIAL PRIMARY KEY,
    requester_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    recipient_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'declined')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    responded_at  TIMESTAMPTZ,
    CHECK (requester_id <> recipient_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS friend_requests_pending_or_accepted_idx
    ON friend_requests (requester_id, recipient_id)
    WHERE status IN ('pending', 'accepted');

-- Speeds up "my incoming requests" and "my friends list" respectively —
-- both filter by one side of the pair plus status, the same shape as
-- migration 0013's feedback_status_idx.
CREATE INDEX IF NOT EXISTS friend_requests_recipient_status_idx ON friend_requests (recipient_id, status);
CREATE INDEX IF NOT EXISTS friend_requests_requester_status_idx ON friend_requests (requester_id, status);
