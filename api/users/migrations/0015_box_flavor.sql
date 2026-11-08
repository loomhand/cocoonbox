-- Every box so far took the one flavor every deployment ran: 4 vCPUs, 8 GiB of memory, a 40 GiB disk.
ALTER TABLE boxes
    ADD COLUMN vcpus integer NOT NULL DEFAULT 4,
    ADD COLUMN memory_mib integer NOT NULL DEFAULT 8192,
    ADD COLUMN disk_gib integer NOT NULL DEFAULT 40;
ALTER TABLE boxes
    ALTER COLUMN vcpus DROP DEFAULT,
    ALTER COLUMN memory_mib DROP DEFAULT,
    ALTER COLUMN disk_gib DROP DEFAULT;
