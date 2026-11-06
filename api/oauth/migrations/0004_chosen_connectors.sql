CREATE TABLE oauth_connectors (
    grant_id      text NOT NULL REFERENCES oauth_grants (id) ON DELETE CASCADE,
    connect_grant text NOT NULL,
    PRIMARY KEY (grant_id, connect_grant)
);

ALTER TABLE oauth_codes ADD COLUMN connectors text[];
