CREATE TABLE boxes (
    id         text PRIMARY KEY,
    owner      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner, name)
);

-- Every user so far has one box, whose id is cb- and the first 12 hex of sha256(user id).
INSERT INTO boxes (id, owner, name, created_at)
SELECT 'cb-' || left(encode(sha256(convert_to(id, 'UTF8')), 'hex'), 12), id, 'main', created_at FROM users;
