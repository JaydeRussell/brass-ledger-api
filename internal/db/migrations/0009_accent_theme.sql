-- Per-account accent-color theme (brass, ultramarine, sanguine, ...) —
-- same cross-device-sync reasoning as migration 0008's theme_preference
-- (light/dark/system), but the frontend's second, independent theming
-- axis: see brass-ledger-web's app/lib/theme.ts's AccentTheme type,
-- which this column's CHECK constraint mirrors exactly. Defaults to
-- 'brass' (today's only theme, and the one every existing account has
-- effectively already had since it shipped); UpsertUserFromGoogle's ON
-- CONFLICT clause never touches this column, so re-signing-in never
-- resets a saved preference.
ALTER TABLE users
    ADD COLUMN accent_theme TEXT NOT NULL DEFAULT 'brass'
        CHECK (accent_theme IN (
            'brass', 'ultramarine', 'sanguine', 'verdant', 'plague-bloom',
            'necron-emerald', 'waaagh', 'amethyst', 'hive-bloom', 'tau-cyan',
            'custodian-gold', 'khorne-crimson'
        ));
