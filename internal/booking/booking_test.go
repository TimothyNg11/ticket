package booking_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"ticket/internal/apperr"
	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/inventory"
	"ticket/internal/payments"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

var signingKey = []byte("ticket-signing-key-ticket-signing-key")

// env is one isolated database with a published event of `seats` seats.
type env struct {
	pool    *pgxpool.Pool
	svc     *booking.Service
	inv     *inventory.Service
	admin   uuid.UUID
	eventID uuid.UUID
	seats   []uuid.UUID // event_seat ids in seat-map order
	price   int32
}

func newEnv(t *testing.T, seats int, pay payments.Client) *env {
	t.Helper()
	pool := pg.NewDB(t)
	inv := inventory.New(pool)
	ctx := context.Background()
	admin := testutil.CreateUser(t, pool, "admin@example.com", "admin")
	v, err := inv.CreateVenue(ctx, admin, inventory.VenueSpec{
		Name: "Hall", Timezone: "UTC",
		Sections: []inventory.SectionSpec{{Name: "Floor", Rows: []inventory.RowSpec{{Label: "A", SeatCount: seats}}}},
	})
	require.NoError(t, err)
	e, err := inv.CreateEvent(ctx, admin, inventory.EventSpec{
		VenueID: v.Venue.ID, Name: "Show", StartsAt: time.Now().Add(48 * time.Hour), OnSaleAt: time.Now().Add(-time.Hour),
		SectionPrices: map[uuid.UUID]int32{v.Sections[0].Section.ID: 2500},
	})
	require.NoError(t, err)
	_, err = inv.PublishEvent(ctx, admin, e.ID)
	require.NoError(t, err)
	sm, err := inv.SeatMap(ctx, e.ID)
	require.NoError(t, err)
	var ids []uuid.UUID
	for _, s := range sm.Sections[0].Seats {
		ids = append(ids, s.EventSeatID)
	}
	return &env{
		pool: pool, inv: inv, admin: admin, eventID: e.ID, seats: ids, price: 2500,
		svc: booking.New(pool, pay, auth.NewTicketSigner(signingKey), 10*time.Minute),
	}
}

var userN atomic.Int64 // tests create users from many goroutines at once

func (e *env) user(t *testing.T) uuid.UUID {
	return testutil.CreateUser(t, e.pool, fmt.Sprintf("u%d-%s@x.com", userN.Add(1), uuid.NewString()[:6]), "user")
}

// seat returns (state, hold_id, order_id, version) of an event seat.
func (e *env) seat(t *testing.T, id uuid.UUID) (state string, hold, order *uuid.UUID, version int) {
	t.Helper()
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT state, hold_id, order_id, version FROM event_seats WHERE id = $1`, id).Scan(&state, &hold, &order, &version))
	return
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func code(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}
