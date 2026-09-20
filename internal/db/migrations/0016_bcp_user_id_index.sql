-- users.bcp_user_id has been a bare, unindexed column since migration
-- 0002 added it, but it's a lookup key on three real paths — the public
-- player dossier (internal/api/dossier.go), sending a friend request,
-- and reading a friend's events (internal/api/friends.go), all via
-- internal/user.GetUserByBcpUserID's `WHERE bcp_user_id = $1`.
--
-- Partial (WHERE bcp_user_id IS NOT NULL) because the column is NULL
-- for every account that hasn't linked a Best Coast Pairings profile
-- yet, and a NULL can never match the equality lookup above — no reason
-- to carry those rows in the index.
--
-- Deliberately NOT UNIQUE, even though GetUserByBcpUserID's own doc
-- comment notes the value is unique "in practice… though nothing in the
-- schema enforces that today". CREATE UNIQUE INDEX fails outright if a
-- duplicate already exists, and a failing migration is fatal at startup
-- (cmd/server/main.go log.Fatalf's on a Migrate error), so it would
-- take the service down rather than surface a warning. Enforcing
-- uniqueness is worth doing as its own deliberate step, after checking
-- the live data:
--
--   SELECT bcp_user_id, count(*) FROM users
--   WHERE bcp_user_id IS NOT NULL GROUP BY 1 HAVING count(*) > 1;
CREATE INDEX IF NOT EXISTS users_bcp_user_id_idx
    ON users (bcp_user_id)
    WHERE bcp_user_id IS NOT NULL;
