-- Access control on top of authentication: having a Google account (and
-- a valid session) is no longer enough to actually use the app, since
-- that's too low a bar for a genuinely public deployment. A new sign-in
-- defaults to role='user', status='pending' — an admin approves/rejects
-- them, or promotes another account to admin, via internal/api/admin.go.
-- See ADMIN_EMAILS (internal/config) for how the very first admin gets
-- bootstrapped without a manual DB step.
ALTER TABLE users
    ADD COLUMN role   TEXT NOT NULL DEFAULT 'user'    CHECK (role IN ('user', 'admin')),
    ADD COLUMN status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected'));

-- Speeds up the admin panel's "show me pending requests" query — cheap
-- either way at this project's scale, but no reason not to have it.
CREATE INDEX IF NOT EXISTS users_status_idx ON users (status);
