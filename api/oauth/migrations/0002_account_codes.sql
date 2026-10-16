ALTER TABLE oauth_codes
    ADD COLUMN account boolean NOT NULL DEFAULT false,
    ADD COLUMN label   text NOT NULL DEFAULT '';
