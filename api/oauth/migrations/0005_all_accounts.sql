ALTER TABLE oauth_grants ADD COLUMN IF NOT EXISTS all_accounts boolean NOT NULL DEFAULT false;
UPDATE oauth_grants g SET all_accounts = true WHERE EXISTS (SELECT 1 FROM oauth_connectors n WHERE n.grant_id = g.id);
DELETE FROM oauth_connectors;

ALTER TABLE oauth_codes ADD COLUMN IF NOT EXISTS all_accounts boolean NOT NULL DEFAULT false;
