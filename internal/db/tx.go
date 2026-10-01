package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/db/sqlc"
)

// InTx runs fn inside one transaction: commit if fn returns nil, roll back otherwise.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *sqlc.Queries) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return fn(sqlc.New(tx))
	})
}

// IsUniqueViolation reports whether err is a Postgres unique_violation (SQLSTATE 23505).
// Callers use it to turn races the database caught into clean 409 responses.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
