-- Links a signed-in account to a Best Coast Pairings user profile, so
-- "my events" (internal/api's /api/me/events) knows whose BCP history to
-- fetch. Nullable and unindexed by design: not every account will link
-- one, there's no lookup by this column (only by the already-indexed
-- primary key via the session), and BCP has no account linkage of its
-- own to backfill from — this is set once, manually, by the user pasting
-- their own BCP profile URL/id (see internal/api's bcp-profile route).
ALTER TABLE users ADD COLUMN IF NOT EXISTS bcp_user_id TEXT;
