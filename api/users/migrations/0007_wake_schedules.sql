CREATE TABLE wake_schedules (
    box          text PRIMARY KEY REFERENCES boxes (id) ON DELETE CASCADE,
    owner        text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cron         text NOT NULL,
    tz           text NOT NULL,
    awake_for_s  integer NOT NULL,
    next_at      timestamptz NOT NULL,
    last_wake_at timestamptz,
    sleep_at     timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
