ALTER TABLE users
    ADD COLUMN image text NOT NULL DEFAULT '',
    ADD COLUMN delta boolean NOT NULL DEFAULT false,
    ADD COLUMN park boolean NOT NULL DEFAULT false,
    ADD COLUMN gc boolean NOT NULL DEFAULT false;
