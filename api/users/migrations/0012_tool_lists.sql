CREATE TABLE tool_lists (
    image        text PRIMARY KEY,
    family       text NOT NULL,
    tools        text NOT NULL,
    instructions text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
