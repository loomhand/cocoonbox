CREATE TABLE invite_tokens (
    id         text PRIMARY KEY,
    sha256     text NOT NULL UNIQUE,
    label      text NOT NULL,
    created_by text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz
);

ALTER TABLE signup_invites ADD COLUMN token_id text REFERENCES invite_tokens (id) ON DELETE SET NULL;
