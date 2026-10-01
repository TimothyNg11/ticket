-- name: InsertAuditLog :exec
INSERT INTO audit_log (actor_id, action, target, metadata) VALUES ($1, $2, $3, $4);
