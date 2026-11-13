CREATE TABLE agent_sessions (
    client   text        NOT NULL,
    box      text        NOT NULL,
    upstream text        NOT NULL,
    read_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client, box)
);

CREATE INDEX agent_sessions_read ON agent_sessions (read_at);
