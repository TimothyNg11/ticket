-- name: CreateVenue :one
INSERT INTO venues (name, timezone) VALUES ($1, $2) RETURNING *;

-- name: GetVenue :one
SELECT * FROM venues WHERE id = $1;

-- name: CreateSection :one
INSERT INTO sections (venue_id, name) VALUES ($1, $2) RETURNING *;

-- name: ListSectionsByVenue :many
SELECT * FROM sections WHERE venue_id = $1 ORDER BY name;

-- Bulk insert through the Postgres COPY protocol: one round trip for thousands of seats.
-- name: CreateSeats :copyfrom
INSERT INTO seats (section_id, row_label, seat_number) VALUES ($1, $2, $3);

-- name: CreateEvent :one
INSERT INTO events (venue_id, name, starts_at, on_sale_at, queue_enabled, admit_batch, admit_interval_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: CreateEventSeatsForSection :execrows
INSERT INTO event_seats (event_id, seat_id, price_cents)
SELECT sqlc.arg(event_id)::uuid, s.id, sqlc.arg(price_cents)::int
FROM seats s WHERE s.section_id = sqlc.arg(section_id)::uuid;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: PublishEvent :one
UPDATE events SET status = 'on_sale' WHERE id = $1 AND status = 'draft' RETURNING *;

-- Keyset pagination: cheaper than OFFSET and stable while new events are added.
-- name: ListPublicEvents :many
SELECT * FROM events
WHERE status <> 'draft'
  AND (starts_at, id) > (sqlc.arg(after_starts_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY starts_at, id
LIMIT sqlc.arg(page_size)::int;

-- name: GetSeatMap :many
SELECT es.id AS event_seat_id, es.price_cents, es.state,
       s.row_label, s.seat_number, sec.id AS section_id, sec.name AS section_name
FROM event_seats es
JOIN seats s      ON s.id = es.seat_id
JOIN sections sec ON sec.id = s.section_id
WHERE es.event_id = $1
ORDER BY sec.name, sec.id, s.row_label, s.seat_number;

-- name: CountAvailableSeats :one
SELECT count(*) FROM event_seats WHERE event_id = $1 AND state = 'available';

-- name: ListOnSaleEventIDs :many
SELECT id FROM events WHERE status = 'on_sale' AND starts_at > now();

-- name: ListQueuedOnSaleEvents :many
SELECT id, admit_batch, admit_interval_seconds FROM events
WHERE status = 'on_sale' AND queue_enabled AND on_sale_at <= now() AND starts_at > now();
