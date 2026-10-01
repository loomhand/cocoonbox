CREATE TABLE users (
    id                  text PRIMARY KEY,
    name                text NOT NULL UNIQUE,
    idp                 text NOT NULL DEFAULT 'password',
    password_hash       text,
    token_sha256        text UNIQUE,
    admin               boolean NOT NULL DEFAULT false,
    disabled            boolean NOT NULL DEFAULT false,
    residential         boolean NOT NULL DEFAULT false,
    max_boxes           integer NOT NULL,
    max_boxes_cap       integer NOT NULL,
    gw_ak_id            text NOT NULL,
    gw_key_sealed       bytea NOT NULL,
    tenant_token_sealed bytea NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id_sha256  text PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
