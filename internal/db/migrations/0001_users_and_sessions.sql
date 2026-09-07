-- Users authenticate via Google Sign-In only for now (see
-- internal/auth/google.go) — google_sub is the "sub" claim from Google's
-- userinfo response, a stable per-account id that survives an email
-- change, so it (not email) is what identifies a returning user.
CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    google_sub    TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    avatar_url    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Server-side sessions rather than a JWT: the session cookie only ever
-- carries an opaque random token, so a session can be revoked (logout,
-- or an admin action later) by deleting its row — a signed JWT can't be
-- un-issued before it expires on its own.
CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);
