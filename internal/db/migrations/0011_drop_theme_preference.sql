-- Reverts 0008_theme_preference.sql: the frontend's light/dark/system
-- toggle was removed (the app is dark-mode-only now, see
-- brass-ledger-web's app/lib/theme.ts) — the accent-color theme
-- (migration 0009) is unaffected and stays. Migrations are an
-- append-only ledger — 0008 stays as history, this just undoes its
-- effect going forward, same as 0006_drop_calendar_token.sql did for a
-- different dropped feature.
ALTER TABLE users DROP COLUMN IF EXISTS theme_preference;
