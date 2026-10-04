package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/db"
	"ticket/internal/db/sqlc"
)

func TestInTxRollsBackOnError(t *testing.T) {
	pool := pg.NewDB(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := db.InTx(ctx, pool, func(q *sqlc.Queries) error {
		_, err := q.CreateUser(ctx, sqlc.CreateUserParams{Email: "a@x.com", PasswordHash: "h", Role: "user"})
		require.NoError(t, err)
		return boom
	})
	assert.ErrorIs(t, err, boom)

	_, err = sqlc.New(pool).GetUserByEmail(ctx, "a@x.com")
	assert.Error(t, err, "insert must have been rolled back")
}

func TestIsUniqueViolation(t *testing.T) {
	pool := pg.NewDB(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	p := sqlc.CreateUserParams{Email: "a@x.com", PasswordHash: "h", Role: "user"}
	_, err := q.CreateUser(ctx, p)
	require.NoError(t, err)
	_, err = q.CreateUser(ctx, p)
	assert.True(t, db.IsUniqueViolation(err))
	assert.False(t, db.IsUniqueViolation(errors.New("other")))
}

// InAsyncCommitTx is for bookkeeping whose loss on a crash is harmless (Phase 9:
// idempotency records). Its commits skip waiting for the WAL flush.
func TestInAsyncCommitTxSkipsWALFlush(t *testing.T) {
	pool := pg.NewDB(t)
	ctx := context.Background()
	var setting string
	require.NoError(t, db.InAsyncCommitTx(ctx, pool, func(q *sqlc.Queries) error {
		return nil
	}))
	err := db.InAsyncCommitTx(ctx, pool, func(q *sqlc.Queries) error {
		return pool.QueryRow(ctx, "SELECT 1").Scan(new(int))
	})
	require.NoError(t, err)
	// Inside the helper's transaction the setting is off; outside it's untouched.
	require.NoError(t, db.InAsyncCommitTxRaw(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SHOW synchronous_commit").Scan(&setting)
	}))
	assert.Equal(t, "off", setting)
	require.NoError(t, pool.QueryRow(ctx, "SHOW synchronous_commit").Scan(&setting))
	assert.Equal(t, "on", setting, "SET LOCAL never leaks to other transactions")
}
