CREATE TABLE vault_secrets (
    id                  text PRIMARY KEY,
    owner               text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name                text NOT NULL,
    hosts               text[] NOT NULL,
    header              text NOT NULL DEFAULT '',
    template            text NOT NULL DEFAULT '',
    query               text NOT NULL DEFAULT '',
    placeholder         text NOT NULL,
    value_sealed        bytea NOT NULL,
    value_hint          text NOT NULL DEFAULT '',
    value_sha256_prefix text NOT NULL,
    value_len           integer NOT NULL,
    version             integer NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner, name)
);

CREATE TABLE vault_audit (
    id        bigserial PRIMARY KEY,
    at        timestamptz NOT NULL DEFAULT now(),
    owner     text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    secret_id text NOT NULL,
    name      text NOT NULL,
    op        text NOT NULL,
    actor     text NOT NULL
);

CREATE INDEX vault_audit_owner ON vault_audit (owner, at);
CREATE INDEX vault_audit_at ON vault_audit (at);
