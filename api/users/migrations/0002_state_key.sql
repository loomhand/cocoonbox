ALTER TABLE users
    ADD COLUMN state_key_id     text,
    ADD COLUMN state_key_sealed bytea;
