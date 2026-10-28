CREATE TABLE oauth_chosen (
    grant_id text NOT NULL REFERENCES oauth_grants (id) ON DELETE CASCADE,
    box_id   text NOT NULL REFERENCES boxes (id) ON DELETE CASCADE,
    PRIMARY KEY (grant_id, box_id)
);

CREATE INDEX oauth_chosen_box ON oauth_chosen (box_id);

ALTER TABLE oauth_codes ADD COLUMN chosen text[];
