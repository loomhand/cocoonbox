CREATE TABLE identities (
    idp            text NOT NULL,
    subject        text NOT NULL,
    user_id        text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email          text NOT NULL,
    email_verified boolean NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (idp, subject),
    UNIQUE (user_id, idp)
);

CREATE TABLE signup_settings (
    id               integer PRIMARY KEY CHECK (id = 1),
    enabled          boolean NOT NULL DEFAULT false,
    invite_only      boolean NOT NULL DEFAULT true,
    seats            integer NOT NULL DEFAULT 0,
    allow_domains    text[] NOT NULL DEFAULT '{}',
    allow_emails     text[] NOT NULL DEFAULT '{}',
    daily_cap_micros bigint NOT NULL DEFAULT 1000000,
    max_agent_boxes  integer NOT NULL DEFAULT 1,
    hourly_accounts  integer NOT NULL DEFAULT 50
);

INSERT INTO signup_settings (id) VALUES (1);

CREATE TABLE signup_invites (
    id          text PRIMARY KEY,
    code_sha256 text NOT NULL UNIQUE,
    uses_left   integer NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE signup_queue (
    id            text PRIMARY KEY,
    idp           text NOT NULL,
    subject       text NOT NULL,
    email         text NOT NULL,
    name_hint     text NOT NULL,
    secret_sha256 text NOT NULL UNIQUE,
    sort_key      timestamptz NOT NULL,
    status        text NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'admitted', 'rejected')),
    invite_id     text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    admitted_at   timestamptz,
    user_id       text REFERENCES users (id) ON DELETE CASCADE,
    UNIQUE (idp, subject)
);

CREATE INDEX signup_queue_waiting ON signup_queue (sort_key, id) WHERE status = 'waiting';

CREATE TABLE login_flows (
    nonce_sha256  text PRIMARY KEY,
    state         text NOT NULL,
    verifier      text NOT NULL,
    link_user     text REFERENCES users (id) ON DELETE CASCADE,
    return_to     text NOT NULL,
    invite_sha256 text NOT NULL,
    expires_at    timestamptz NOT NULL
);
