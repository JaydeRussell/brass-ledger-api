-- Whether an approved account's player dossier (name + faction/placing
-- summary, built from the same data GET /api/players/:bcpUserId/stats
-- already exposes to any signed-in user) is reachable by anyone, signed
-- in or not, at GET /api/players/:bcpUserId/dossier — see
-- internal/api/dossier.go. Defaults to true (visible) per product
-- decision: every already-linked account's dossier is public from the
-- moment this ships, with an opt-out (POST /api/me/dossier-visibility)
-- rather than an opt-in. UpsertUserFromGoogle's ON CONFLICT clause never
-- touches this column, so re-signing-in never resets a saved preference
-- — same pattern as migration 0009's accent_theme.
ALTER TABLE users
    ADD COLUMN dossier_public BOOLEAN NOT NULL DEFAULT true;
