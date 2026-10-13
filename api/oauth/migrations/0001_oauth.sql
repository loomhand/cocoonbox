CREATE TABLE oauth_key (
    id     integer PRIMARY KEY CHECK (id = 1),
    secret bytea NOT NULL
);

CREATE TABLE oauth_codes (
    code_sha256      text PRIMARY KEY,
    user_id          text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client_id        text NOT NULL,
    client_name      text NOT NULL,
    client_host      text NOT NULL,
    redirect_uri     text NOT NULL,
    code_challenge   text NOT NULL,
    max_boxes        integer NOT NULL,
    max_running      integer NOT NULL,
    grant_expires_at timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL
);

CREATE TABLE oauth_grants (
    id          text PRIMARY KEY,
    user_id     text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client_id   text NOT NULL,
    client_name text NOT NULL,
    client_host text NOT NULL,
    max_boxes   integer NOT NULL,
    max_running integer NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    seen        boolean NOT NULL DEFAULT false
);

CREATE INDEX oauth_grants_user ON oauth_grants (user_id);

CREATE TABLE oauth_tokens (
    sha256     text PRIMARY KEY,
    grant_id   text NOT NULL REFERENCES oauth_grants (id) ON DELETE CASCADE,
    refresh    boolean NOT NULL,
    spent      boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL
);

CREATE INDEX oauth_tokens_grant ON oauth_tokens (grant_id);

-- A box keeps its row after its grant goes, so it still hibernates when idle.
CREATE TABLE oauth_boxes (
    box_id   text PRIMARY KEY REFERENCES boxes (id) ON DELETE CASCADE,
    grant_id text REFERENCES oauth_grants (id) ON DELETE SET NULL
);

CREATE INDEX oauth_boxes_grant ON oauth_boxes (grant_id);
