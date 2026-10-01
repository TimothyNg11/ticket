CREATE TABLE venues (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    timezone   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sections (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    venue_id uuid NOT NULL REFERENCES venues (id) ON DELETE CASCADE,
    name     text NOT NULL,
    UNIQUE (venue_id, name)
);

CREATE TABLE seats (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id  uuid NOT NULL REFERENCES sections (id) ON DELETE CASCADE,
    row_label   text NOT NULL,
    seat_number int  NOT NULL CHECK (seat_number > 0),
    UNIQUE (section_id, row_label, seat_number)
);

CREATE TABLE events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    venue_id   uuid NOT NULL REFERENCES venues (id),
    name       text NOT NULL,
    starts_at  timestamptz NOT NULL,
    on_sale_at timestamptz NOT NULL,
    status     text NOT NULL DEFAULT 'draft'
               CHECK (status IN ('draft', 'on_sale', 'sold_out', 'ended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (on_sale_at < starts_at)
);
-- Public listing pages through non-draft events by (starts_at, id).
CREATE INDEX events_public_listing_idx ON events (starts_at, id) WHERE status <> 'draft';

CREATE TABLE event_seats (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    uuid NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    seat_id     uuid NOT NULL REFERENCES seats (id),
    price_cents int  NOT NULL CHECK (price_cents > 0),
    state       text NOT NULL DEFAULT 'available' CHECK (state IN ('available', 'held', 'sold')),
    -- Foreign keys to holds and orders are added in Phase 2, when those tables exist.
    hold_id     uuid,
    order_id    uuid,
    version     int  NOT NULL DEFAULT 0,
    UNIQUE (event_id, seat_id),
    -- The database, not just the application, guarantees a seat's references match its state.
    CONSTRAINT event_seats_state_refs CHECK (
        (state = 'available' AND hold_id IS NULL     AND order_id IS NULL) OR
        (state = 'held'      AND hold_id IS NOT NULL AND order_id IS NULL) OR
        (state = 'sold'      AND hold_id IS NULL     AND order_id IS NOT NULL)
    )
);
CREATE INDEX event_seats_event_state_idx ON event_seats (event_id, state);
