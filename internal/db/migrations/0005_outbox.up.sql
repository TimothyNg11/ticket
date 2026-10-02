-- Transactional outbox: domain events are inserted in the same transaction as the
-- change they describe, then a relay publishes them. If the process dies after the
-- commit, the event is still here waiting; it can never be lost or published for
-- a change that rolled back.
CREATE TABLE outbox (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_id uuid        NOT NULL,
    event_type   text        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
-- The relay only ever scans unpublished rows, so index just those.
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;
