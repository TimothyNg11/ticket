-- name: ClaimIdempotencyKey :execrows
-- Inserting first is what makes concurrent retries safe: only one request can
-- create the row, so only one runs the handler.
INSERT INTO idempotency_keys (user_id, key, request_hash, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, key) DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys WHERE user_id = $1 AND key = $2;

-- name: TakeOverIdempotencyKey :execrows
-- An in-progress row older than the cutoff belongs to a request that died; let the
-- retry claim it. Only one retry can win the conditional update.
UPDATE idempotency_keys SET created_at = now()
WHERE user_id = $1 AND key = $2 AND response_status IS NULL AND created_at < sqlc.arg(cutoff)::timestamptz;

-- name: SaveIdempotentResponse :exec
UPDATE idempotency_keys SET response_status = $3, response_body = $4 WHERE user_id = $1 AND key = $2;

-- name: DeleteIdempotencyKey :exec
DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2;

-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE expires_at < now();
