CREATE TABLE snapshot_pins (
    id         text        PRIMARY KEY,
    user_id    text        NOT NULL,
    box        text        NOT NULL,
    snapshot   text        NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE INDEX snapshot_pins_box ON snapshot_pins (user_id, box);
