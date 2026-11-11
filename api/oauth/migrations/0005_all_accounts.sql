ALTER TABLE oauth_grants ADD COLUMN IF NOT EXISTS all_accounts boolean NOT NULL DEFAULT false;
DELETE FROM oauth_connectors;

ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS all_accounts boolean NOT NULL DEFAULT false;
ALTER TABLE oauth_codes DROP COLUMN IF EXISTS connectors;
