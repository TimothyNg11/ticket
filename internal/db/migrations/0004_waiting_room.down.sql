ALTER TABLE events
    DROP COLUMN queue_enabled,
    DROP COLUMN admit_batch,
    DROP COLUMN admit_interval_seconds;
