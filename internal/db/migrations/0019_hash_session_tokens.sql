-- Sessions store a SHA-256 hex digest of the token instead of the token
-- itself (internal/user/store.go's hashSessionToken), so a copy of the
-- database holds no usable sessions. Hashing the existing rows in place
-- keeps everyone signed in. Expired rows, which nothing used to remove,
-- go at the same time.
DELETE FROM sessions WHERE expires_at <= now();
UPDATE sessions SET token = encode(sha256(convert_to(token, 'UTF8')), 'hex');
