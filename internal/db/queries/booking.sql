-- name: LockAvailableSeats :many
-- SKIP LOCKED: a seat another transaction is claiming right now counts as
-- unavailable instead of making us wait for a lock we'd almost certainly lose.
-- ORDER BY id gives every transaction the same lock order.
SELECT id FROM event_seats
WHERE event_id = sqlc.arg(event_id)::uuid AND id = ANY(sqlc.arg(seat_ids)::uuid[]) AND state = 'available'
ORDER BY id
FOR UPDATE SKIP LOCKED;

-- name: CreateHold :one
INSERT INTO holds (event_id, user_id, expires_at) VALUES ($1, $2, $3) RETURNING *;

-- name: HoldSeats :execrows
UPDATE event_seats SET state = 'held', hold_id = sqlc.arg(hold_id)::uuid, version = version + 1
WHERE id = ANY(sqlc.arg(seat_ids)::uuid[]) AND state = 'available';

-- name: GetHoldForUpdate :one
SELECT * FROM holds WHERE id = $1 FOR UPDATE;

-- name: SetHoldStatus :exec
UPDATE holds SET status = $2 WHERE id = $1;

-- name: ReleaseHoldSeats :execrows
UPDATE event_seats SET state = 'available', hold_id = NULL, version = version + 1
WHERE hold_id = $1 AND state = 'held';

-- name: ListHoldSeats :many
SELECT id, price_cents FROM event_seats WHERE hold_id = $1 ORDER BY id;

-- name: HasPendingOrderForHold :one
SELECT EXISTS (SELECT 1 FROM orders WHERE hold_id = $1 AND status = 'pending_payment');

-- name: LockExpiredHolds :many
-- Holds with a payment in flight are skipped: the buyer may already have been
-- charged, so their seats must not be released under them.
SELECT h.id FROM holds h
WHERE h.status = 'active' AND h.expires_at < now()
  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.hold_id = h.id AND o.status = 'pending_payment')
ORDER BY h.expires_at
LIMIT sqlc.arg(batch)::int
FOR UPDATE SKIP LOCKED;

-- name: ExpireHolds :exec
UPDATE holds SET status = 'expired' WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ReleaseSeatsOfHolds :execrows
UPDATE event_seats SET state = 'available', hold_id = NULL, version = version + 1
WHERE hold_id = ANY(sqlc.arg(ids)::uuid[]) AND state = 'held';

-- name: CreateOrder :one
INSERT INTO orders (user_id, event_id, hold_id, total_cents, status, idempotency_key)
VALUES ($1, $2, $3, $4, 'pending_payment', $5) RETURNING *;

-- name: GetOrderByKey :one
SELECT * FROM orders WHERE user_id = $1 AND idempotency_key = $2;

-- name: GetOrder :one
SELECT * FROM orders WHERE id = $1;

-- name: GetOrderForUpdate :one
SELECT * FROM orders WHERE id = $1 FOR UPDATE;

-- name: SetOrderStatus :exec
UPDATE orders SET status = $2, updated_at = now() WHERE id = $1;

-- name: ListOrdersByUser :many
SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: SellHoldSeats :execrows
UPDATE event_seats SET state = 'sold', hold_id = NULL, order_id = sqlc.arg(order_id)::uuid, version = version + 1
WHERE hold_id = sqlc.arg(hold_id)::uuid AND state = 'held';

-- name: ReleaseOrderSeats :execrows
UPDATE event_seats SET state = 'available', order_id = NULL, version = version + 1
WHERE order_id = $1 AND state = 'sold';

-- name: CreateTicket :one
INSERT INTO tickets (id, order_id, event_seat_id, qr_token) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: ListTicketsByOrders :many
SELECT * FROM tickets WHERE order_id = ANY(sqlc.arg(order_ids)::uuid[]) ORDER BY created_at, id;

-- name: VoidOrderTickets :exec
UPDATE tickets SET status = 'void' WHERE order_id = $1 AND status = 'valid';

-- name: CreatePayment :exec
INSERT INTO payments (order_id, kind, provider_ref, amount_cents, status, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: GetChargeRef :one
SELECT provider_ref FROM payments WHERE order_id = $1 AND kind = 'charge' AND status = 'succeeded';
