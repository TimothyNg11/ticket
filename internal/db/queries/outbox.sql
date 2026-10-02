-- name: InsertOutbox :exec
INSERT INTO outbox (aggregate_id, event_type, payload) VALUES ($1, $2, $3);

-- SKIP LOCKED lets several relay replicas drain the outbox without sending the
-- same row twice at the same time.
-- name: ClaimOutbox :many
SELECT id, aggregate_id, event_type, payload, created_at FROM outbox
WHERE published_at IS NULL
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxPublished :exec
UPDATE outbox SET published_at = now() WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: OutboxLag :one
-- Age in seconds of the oldest unpublished event (0 when empty); alerting watches it.
SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8 FROM outbox WHERE published_at IS NULL;

-- name: ListStalePendingOrders :many
SELECT * FROM orders WHERE status = 'pending_payment' AND updated_at < $1 ORDER BY updated_at LIMIT $2;

-- name: ListUnrefundedCancelledOrders :many
SELECT * FROM orders WHERE status = 'cancelled' AND updated_at < $1 ORDER BY updated_at LIMIT $2;
