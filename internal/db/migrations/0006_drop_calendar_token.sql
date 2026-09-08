-- Reverts 0005_calendar_token.sql: the subscribable-calendar-feed
-- feature it supported was dropped before shipping (an in-app month
-- view stayed; only the .ics subscription link went away). Migrations
-- are an append-only ledger — 0005 stays as history, this just undoes
-- its effect going forward, the same way any other schema change would
-- be walked back once already applied.
DROP INDEX IF EXISTS idx_users_calendar_token;
ALTER TABLE users DROP COLUMN IF EXISTS calendar_token;
