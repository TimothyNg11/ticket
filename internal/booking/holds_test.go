package booking_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateHold(t *testing.T) {
	e := newEnv(t, 5, nil)
	u := e.user(t)
	h, err := e.svc.CreateHold(context.Background(), u, e.eventID, e.seats[:2])
	require.NoError(t, err)
	assert.Equal(t, "active", h.Status)
	assert.ElementsMatch(t, e.seats[:2], h.SeatIDs)
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), h.ExpiresAt, 5*time.Second)

	for _, id := range e.seats[:2] {
		state, hold, _, version := e.seat(t, id)
		assert.Equal(t, "held", state)
		assert.Equal(t, h.ID, *hold)
		assert.Equal(t, 1, version)
	}
	state, _, _, _ := e.seat(t, e.seats[2])
	assert.Equal(t, "available", state)
}

func TestCreateHoldSeatTaken(t *testing.T) {
	e := newEnv(t, 5, nil)
	ctx := context.Background()
	_, err := e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[:2])
	require.NoError(t, err)

	_, err = e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[1:3])
	assert.Equal(t, "SEAT_UNAVAILABLE", code(err))
	assert.ErrorContains(t, err, e.seats[1].String())
	state, _, _, _ := e.seat(t, e.seats[2])
	assert.Equal(t, "available", state, "a failed hold changes nothing")
}

func TestCreateHoldRejectsSeatsFromOtherEvent(t *testing.T) {
	e := newEnv(t, 3, nil)
	_, err := e.svc.CreateHold(context.Background(), e.user(t), e.eventID, []uuid.UUID{uuid.New()})
	assert.Equal(t, "SEAT_UNAVAILABLE", code(err))
}

func TestCreateHoldNotOnSale(t *testing.T) {
	e := newEnv(t, 3, nil)
	ctx := context.Background()
	_, err := e.svc.CreateHold(ctx, e.user(t), uuid.New(), e.seats[:1])
	assert.Equal(t, "NOT_FOUND", code(err))

	_, err = e.pool.Exec(ctx, `UPDATE events SET on_sale_at = now() + interval '1 hour' WHERE id = $1`, e.eventID)
	require.NoError(t, err)
	_, err = e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[:1])
	assert.Equal(t, "NOT_ON_SALE", code(err))

	_, err = e.pool.Exec(ctx, `UPDATE events SET status = 'draft' WHERE id = $1`, e.eventID)
	require.NoError(t, err)
	_, err = e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[:1])
	assert.Equal(t, "NOT_FOUND", code(err))
}

func TestOneActiveHoldPerUser(t *testing.T) {
	e := newEnv(t, 5, nil)
	ctx := context.Background()
	u := e.user(t)
	_, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	_, err = e.svc.CreateHold(ctx, u, e.eventID, e.seats[1:2])
	assert.Equal(t, "HOLD_EXISTS", code(err))
	state, _, _, _ := e.seat(t, e.seats[1])
	assert.Equal(t, "available", state)
}

func TestReleaseHold(t *testing.T) {
	e := newEnv(t, 5, nil)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:2])
	require.NoError(t, err)

	assert.Equal(t, "NOT_FOUND", code(e.svc.ReleaseHold(ctx, e.user(t), h.ID)), "only the owner can release")
	require.NoError(t, e.svc.ReleaseHold(ctx, u, h.ID))
	for _, id := range e.seats[:2] {
		state, hold, _, version := e.seat(t, id)
		assert.Equal(t, "available", state)
		assert.Nil(t, hold)
		assert.Equal(t, 2, version)
	}
	assert.Equal(t, "HOLD_NOT_ACTIVE", code(e.svc.ReleaseHold(ctx, u, h.ID)))

	// Releasing frees the user to hold again.
	_, err = e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	assert.NoError(t, err)
}
