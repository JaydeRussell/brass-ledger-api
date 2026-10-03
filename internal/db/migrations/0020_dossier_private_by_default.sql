-- New accounts start with their public dossier off, so nothing about them
-- (including the account name on the dossier) is published until they
-- turn it on. Existing accounts keep whatever they have now: this only
-- changes the column default, not any row.
ALTER TABLE users ALTER COLUMN dossier_public SET DEFAULT false;
