-- Per-account theme preference (light/dark/system), so the redesign's
-- new theme toggle follows a signed-in user across devices instead of
-- staying stuck in localStorage on whichever device set it — same reason
-- follows/recent-events sync to an account instead of staying purely
-- local. Defaults to 'system' (today's behavior for anyone who's never
-- touched the toggle); UpsertUserFromGoogle's ON CONFLICT clause never
-- touches this column, so re-signing-in never resets a saved preference.
ALTER TABLE users
    ADD COLUMN theme_preference TEXT NOT NULL DEFAULT 'system'
        CHECK (theme_preference IN ('light', 'dark', 'system'));
