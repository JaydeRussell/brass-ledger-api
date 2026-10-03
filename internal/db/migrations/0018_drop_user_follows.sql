-- Following teams/players is no longer a feature, so nothing reads or
-- writes user_follows. Dropping it removes which players and teams each
-- account followed, rather than keeping personal data with no use.
DROP TABLE IF EXISTS user_follows;
