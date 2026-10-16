CREATE TABLE user_tokens (
    id         text PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    sha256     text NOT NULL UNIQUE,
    client     text NOT NULL,
    label      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz
);

CREATE INDEX user_tokens_user ON user_tokens (user_id);
