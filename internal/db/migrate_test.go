package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/db"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

func TestMigrationsRoundTrip(t *testing.T) {
	pool := pg.NewDB(t)
	url := pg.URL(t, pool)
	pool.Close() // migrate uses its own connection; drop ours so DROP TABLE isn't blocked

	require.NoError(t, db.MigrateDown(url))
	require.NoError(t, db.Migrate(url))
	require.NoError(t, db.Migrate(url), "second Migrate must be a no-op")
}

func TestEventSeatStateConstraint(t *testing.T) {
	pool := pg.NewDB(t)
	ctx := context.Background()
	var eventID, seatID string
	require.NoError(t, pool.QueryRow(ctx, `
		WITH v AS (INSERT INTO venues (name, timezone) VALUES ('V', 'UTC') RETURNING id),
		     s AS (INSERT INTO sections (venue_id, name) SELECT id, 'A' FROM v RETURNING id),
		     st AS (INSERT INTO seats (section_id, row_label, seat_number) SELECT id, '1', 1 FROM s RETURNING id),
		     e AS (INSERT INTO events (venue_id, name, starts_at, on_sale_at)
		           SELECT id, 'E', now() + interval '2 days', now() FROM v RETURNING id)
		SELECT (SELECT id FROM e)::text, (SELECT id FROM st)::text`).Scan(&eventID, &seatID))

	// A held seat without a hold_id violates the state/reference check.
	_, err := pool.Exec(ctx,
		`INSERT INTO event_seats (event_id, seat_id, price_cents, state) VALUES ($1, $2, 100, 'held')`,
		eventID, seatID)
	assert.ErrorContains(t, err, "event_seats_state_refs")

	_, err = pool.Exec(ctx,
		`INSERT INTO event_seats (event_id, seat_id, price_cents) VALUES ($1, $2, 100)`, eventID, seatID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO event_seats (event_id, seat_id, price_cents) VALUES ($1, $2, 100)`, eventID, seatID)
	assert.ErrorContains(t, err, "duplicate key", "one row per seat per event")
}
