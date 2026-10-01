CREATE TABLE holds (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   uuid NOT NULL REFERENCES events (id),
    user_id    uuid NOT NULL REFERENCES users (id),
    expires_at timestamptz NOT NULL,
    status     text NOT NULL DEFAULT 'active'
               CHECK (status IN ('active', 'converted', 'expired', 'released')),
    created_at timestamptz NOT NULL DEFAULT now()
);
-- The expiry sweeper scans active holds by expiry time.
CREATE INDEX holds_sweep_idx ON holds (status, expires_at);
-- One active hold per user per event, so one account can't hoard seats 8 at a time.
CREATE UNIQUE INDEX holds_one_active_per_user ON holds (event_id, user_id) WHERE status = 'active';

CREATE TABLE orders (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users (id),
    event_id        uuid NOT NULL REFERENCES events (id),
    hold_id         uuid NOT NULL REFERENCES holds (id),
    total_cents     int  NOT NULL CHECK (total_cents > 0),
    status          text NOT NULL
                    CHECK (status IN ('pending_payment', 'confirmed', 'failed', 'cancelled', 'refunded')),
    -- Client-chosen, so unique per user rather than globally.
    idempotency_key text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);
-- A hold has at most one live order; a failed payment doesn't block a retry.
CREATE UNIQUE INDEX orders_one_live_per_hold ON orders (hold_id) WHERE status <> 'failed';
CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC);

CREATE TABLE payments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id        uuid NOT NULL REFERENCES orders (id),
    kind            text NOT NULL CHECK (kind IN ('charge', 'refund')),
    provider_ref    text,
    amount_cents    int  NOT NULL,
    status          text NOT NULL CHECK (status IN ('succeeded', 'declined')),
    idempotency_key text NOT NULL UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tickets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id      uuid NOT NULL REFERENCES orders (id),
    event_seat_id uuid NOT NULL REFERENCES event_seats (id),
    status        text NOT NULL DEFAULT 'valid' CHECK (status IN ('valid', 'void')),
    qr_token      text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
-- The last line of defense against double-selling: one valid ticket per seat, ever.
-- Cancelled tickets are voided (kept for history), which frees the seat for resale.
CREATE UNIQUE INDEX tickets_one_valid_per_seat ON tickets (event_seat_id) WHERE status = 'valid';
CREATE INDEX tickets_order_idx ON tickets (order_id);

CREATE TABLE idempotency_keys (
    user_id         uuid NOT NULL REFERENCES users (id),
    key             text NOT NULL,
    request_hash    text NOT NULL,
    -- NULL while the first request is still running.
    response_status int,
    response_body   bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);

ALTER TABLE event_seats
    ADD CONSTRAINT event_seats_hold_fk FOREIGN KEY (hold_id) REFERENCES holds (id),
    ADD CONSTRAINT event_seats_order_fk FOREIGN KEY (order_id) REFERENCES orders (id);
CREATE INDEX event_seats_hold_idx ON event_seats (hold_id) WHERE hold_id IS NOT NULL;
CREATE INDEX event_seats_order_idx ON event_seats (order_id) WHERE order_id IS NOT NULL;
