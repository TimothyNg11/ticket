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

// InAsyncCommitTx is InTx for bookkeeping that is safe to lose in a database
// crash: its commit doesn't wait for the write-ahead log to reach disk
// (synchronous_commit = off for this transaction only). Load tests showed the
// database bound by WAL flushes (Phase 9), and idempotency records were the
// most frequent commits. Losing one means a retried request runs again, which
// the orders table's own unique key still deduplicates. Never use it for
// orders, payments, tickets or seats.
func InAsyncCommitTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *sqlc.Queries) error) error {
	return InAsyncCommitTxRaw(ctx, pool, func(tx pgx.Tx) error { return fn(sqlc.New(tx)) })
}

// InAsyncCommitTxRaw is InAsyncCommitTx with the raw transaction.
func InAsyncCommitTxRaw(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
			return err
		}
		return fn(tx)
	})
}
