ALTER TABLE users
    ADD COLUMN image text NOT NULL DEFAULT '',
    ADD COLUMN delta boolean NOT NULL DEFAULT false,
    ADD COLUMN no_park boolean NOT NULL DEFAULT false,
    ADD COLUMN no_gc boolean NOT NULL DEFAULT false;
