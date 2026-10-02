CREATE TABLE shares (
    id            text PRIMARY KEY,
    box           text NOT NULL,
    owner         text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    secret_sha256 text NOT NULL UNIQUE,
    user_ids      text[] NOT NULL DEFAULT '{}',
    write         boolean NOT NULL DEFAULT false,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz
);

CREATE UNIQUE INDEX shares_live_box ON shares (box) WHERE revoked_at IS NULL;
