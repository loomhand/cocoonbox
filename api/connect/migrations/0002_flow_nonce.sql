DELETE FROM connect_flows;
ALTER TABLE connect_flows ADD COLUMN nonce_sha256 text NOT NULL;
