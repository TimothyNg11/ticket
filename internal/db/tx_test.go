package db_test

import (
	"context"
	"errors"
	"testing"

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
