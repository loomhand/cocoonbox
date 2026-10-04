CREATE TABLE connect_grants (
    id               text PRIMARY KEY,
    user_id          text NOT NULL,
    connector        text NOT NULL,
    kind             text NOT NULL,
    mcp_url          text NOT NULL,
    account_label    text NOT NULL DEFAULT '',
    scopes           text[] NOT NULL DEFAULT '{}',
    status           text NOT NULL DEFAULT 'active',
    policy           jsonb NOT NULL DEFAULT '{}',
    sealed           bytea NOT NULL,
    token_expires_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, connector)
);

CREATE TABLE connect_flows (
    id             text PRIMARY KEY,
    user_id        text NOT NULL,
    connector      text NOT NULL,
    kind           text NOT NULL,
    mcp_url        text NOT NULL,
    state_sha256   text NOT NULL UNIQUE,
    session_sha256 text NOT NULL,
    sealed         bytea NOT NULL,
    expires_at     timestamptz NOT NULL
);

CREATE TABLE connect_approvals (
    id          text PRIMARY KEY,
    user_id     text NOT NULL,
    grant_id    text NOT NULL REFERENCES connect_grants (id) ON DELETE CASCADE,
    tool        text NOT NULL,
    args_sha256 text NOT NULL,
    summary     text NOT NULL,
    decision    text NOT NULL DEFAULT 'pending',
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);

CREATE TABLE connect_audit (
    id              bigserial PRIMARY KEY,
    at              timestamptz NOT NULL DEFAULT now(),
    user_id         text NOT NULL,
    connector       text NOT NULL,
    tool            text NOT NULL,
    door            text NOT NULL,
    caller          text NOT NULL,
    decision        text NOT NULL,
    outcome         text NOT NULL,
    upstream_status integer NOT NULL DEFAULT 0,
    latency_ms      bigint NOT NULL DEFAULT 0,
    req_bytes       bigint NOT NULL DEFAULT 0,
    resp_bytes      bigint NOT NULL DEFAULT 0,
    args_sha256     text NOT NULL DEFAULT ''
);

CREATE INDEX connect_audit_user ON connect_audit (user_id, connector, at);
CREATE INDEX connect_audit_at ON connect_audit (at);
