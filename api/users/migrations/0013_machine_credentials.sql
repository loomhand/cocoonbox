CREATE TABLE machine_keys (
    ak_id       text PRIMARY KEY,
    machine_id  text NOT NULL,
    user_id     text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    incarnation text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX machine_keys_machine ON machine_keys (machine_id);

CREATE TABLE credential_pushes (
    machine_id text NOT NULL,
    kind       text NOT NULL,
    node       text NOT NULL,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    pushed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (machine_id, kind, node)
);

CREATE INDEX credential_pushes_node ON credential_pushes (node);
