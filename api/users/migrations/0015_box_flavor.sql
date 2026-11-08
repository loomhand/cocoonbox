-- Every box so far took the one flavor every deployment ran: 4 vCPUs, 8 GiB of memory, a 40 GiB disk. The defaults stay
-- for the images before this one, which insert boxes without the columns during a rolling deploy or after a rollback.
ALTER TABLE boxes
    ADD COLUMN vcpus integer NOT NULL DEFAULT 4,
    ADD COLUMN memory_mib integer NOT NULL DEFAULT 8192,
    ADD COLUMN disk_gib integer NOT NULL DEFAULT 40;
