-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4) RETURNING *;

-- Locks the row so two concurrent refreshes with the same token serialize:
-- the second one sees it revoked and is treated as reuse.
-- name: GetRefreshTokenForUpdate :one
SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now(), replaced_by = $2 WHERE id = $1;

-- name: RevokeRefreshTokenFamilyByHash :exec
UPDATE refresh_tokens SET revoked_at = now()
WHERE revoked_at IS NULL
  AND family_id = (SELECT family_id FROM refresh_tokens t WHERE t.token_hash = $1);

-- name: RevokeRefreshTokenFamily :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL;
