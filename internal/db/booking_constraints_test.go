package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bookingFixture inserts one user, event, seat and hold, returning their ids.
func bookingFixture(t *testing.T, pool *pgxpool.Pool) (userID, eventID, eventSeatID, holdID string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(), `
		WITH u AS (INSERT INTO users (email, password_hash) VALUES ('a@x.com', 'h') RETURNING id),
		     v AS (INSERT INTO venues (name, timezone) VALUES ('V', 'UTC') RETURNING id),
		     s AS (INSERT INTO sections (venue_id, name) SELECT id, 'A' FROM v RETURNING id),
		     st AS (INSERT INTO seats (section_id, row_label, seat_number) SELECT id, '1', 1 FROM s RETURNING id),
		     e AS (INSERT INTO events (venue_id, name, starts_at, on_sale_at)
		           SELECT id, 'E', now() + interval '2 days', now() FROM v RETURNING id),
		     es AS (INSERT INTO event_seats (event_id, seat_id, price_cents)
		            SELECT e.id, st.id, 100 FROM e, st RETURNING id),
		     h AS (INSERT INTO holds (event_id, user_id, expires_at)
		           SELECT e.id, u.id, now() + interval '10 minutes' FROM e, u RETURNING id)
		SELECT (SELECT id FROM u)::text, (SELECT id FROM e)::text, (SELECT id FROM es)::text, (SELECT id FROM h)::text`,
	).Scan(&userID, &eventID, &eventSeatID, &holdID))
	return
}

func TestBookingConstraints(t *testing.T) {
	pool := pg.NewDB(t)
	ctx := context.Background()
	userID, eventID, seatID, holdID := bookingFixture(t, pool)

	order := func(key, status string) (string, error) {
		var id string
		err := pool.QueryRow(ctx, `INSERT INTO orders (user_id, event_id, hold_id, total_cents, status, idempotency_key)
			VALUES ($1, $2, $3, 100, $4, $5) RETURNING id`, userID, eventID, holdID, status, key).Scan(&id)
		return id, err
	}

	// A failed order doesn't block a retry on the same hold; a second live one is rejected.
	_, err := order("k1", "failed")
	require.NoError(t, err)
	live, err := order("k2", "pending_payment")
	require.NoError(t, err)
	_, err = order("k3", "pending_payment")
	assert.ErrorContains(t, err, "orders_one_live_per_hold")

	// Idempotency keys are unique per user, not globally.
	_, err = order("k1", "failed")
	assert.ErrorContains(t, err, "duplicate key", "same user, same key")
	var otherUser string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('b@x.com','h') RETURNING id`).Scan(&otherUser))
	_, err = pool.Exec(ctx, `INSERT INTO orders (user_id, event_id, hold_id, total_cents, status, idempotency_key)
		VALUES ($1, $2, $3, 100, 'failed', 'k1')`, otherUser, eventID, holdID)
	assert.NoError(t, err, "another user may reuse the same key")

	// One valid ticket per seat; voiding the old one lets the seat be resold.
	ticket := func(status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO tickets (order_id, event_seat_id, status, qr_token) VALUES ($1, $2, $3, 'qr')`,
			live, seatID, status)
		return err
	}
	require.NoError(t, ticket("valid"))
	assert.ErrorContains(t, ticket("valid"), "tickets_one_valid_per_seat")
	_, err = pool.Exec(ctx, `UPDATE tickets SET status = 'void' WHERE event_seat_id = $1`, seatID)
	require.NoError(t, err)
	assert.NoError(t, ticket("valid"))

	// One active hold per user per event.
	_, err = pool.Exec(ctx, `INSERT INTO holds (event_id, user_id, expires_at) VALUES ($1, $2, now())`, eventID, userID)
	assert.ErrorContains(t, err, "holds_one_active_per_user")

	// event_seats.hold_id must reference a real hold.
	_, err = pool.Exec(ctx, `UPDATE event_seats SET state = 'held', hold_id = gen_random_uuid() WHERE id = $1`, seatID)
	assert.ErrorContains(t, err, "foreign key")
}
