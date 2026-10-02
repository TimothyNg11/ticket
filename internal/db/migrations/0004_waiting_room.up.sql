-- Waiting-room settings for high-demand events. When queue_enabled is set, holds
-- require an admission pass, and the admitter lets admit_batch users in every
-- admit_interval_seconds.
ALTER TABLE events
    ADD COLUMN queue_enabled          boolean NOT NULL DEFAULT false,
    ADD COLUMN admit_batch            int     NOT NULL DEFAULT 500 CHECK (admit_batch > 0),
    ADD COLUMN admit_interval_seconds int     NOT NULL DEFAULT 10  CHECK (admit_interval_seconds > 0);
