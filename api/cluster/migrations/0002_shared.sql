CREATE SEQUENCE machines_version;

ALTER TABLE machines ALTER COLUMN version SET DEFAULT nextval('machines_version');

ALTER TABLE machines ADD COLUMN env jsonb NOT NULL DEFAULT '{}';
