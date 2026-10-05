CREATE TABLE connect_door_keys (
    key_sha256  text PRIMARY KEY,
    machine_id  text NOT NULL,
    user_id     text NOT NULL,
    incarnation text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX connect_door_keys_machine ON connect_door_keys (machine_id);
