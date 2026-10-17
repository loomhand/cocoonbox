CREATE TABLE machines (
    id             text        PRIMARY KEY,
    owner          text        NOT NULL,
    image          text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    claim          json,
    owner_addr     text        NOT NULL DEFAULT '',
    op             text        NOT NULL DEFAULT '',
    op_err         text        NOT NULL DEFAULT '',
    op_until       timestamptz,
    revision       bigint      NOT NULL DEFAULT 0,
    meta           jsonb       NOT NULL DEFAULT '{}',
    wakes          integer     NOT NULL DEFAULT 0,
    routed         boolean     NOT NULL DEFAULT false,
    ready          boolean     NOT NULL DEFAULT false,
    ready_restarts integer     NOT NULL DEFAULT 0,
    version        bigint      NOT NULL DEFAULT 1,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX machines_owner ON machines (owner);

CREATE TABLE box_heads (
    user_id    text        NOT NULL,
    box        text        NOT NULL,
    head       json        NOT NULL,
    version    bigint      NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, box)
);

CREATE TABLE operations (
    id          text        PRIMARY KEY,
    user_id     text        NOT NULL,
    box         text        NOT NULL,
    phase       integer     NOT NULL,
    park        boolean     NOT NULL DEFAULT false,
    gen         bigint      NOT NULL DEFAULT 0,
    events      json        NOT NULL DEFAULT '[]',
    finished_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX operations_active ON operations (box) WHERE finished_at IS NULL;
CREATE INDEX operations_finished ON operations (finished_at);
