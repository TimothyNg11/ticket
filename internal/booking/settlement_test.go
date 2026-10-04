package booking_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/booking"
	"ticket/internal/payments"
	"ticket/internal/payments/mock"
)

// A checkout that commits its order after the sweeper's query took its snapshot,
// but before the sweeper reached the hold row, is invisible to that query; and the
// checkout only locked the hold row (never updated it), so Postgres doesn't
// re-evaluate it. The hook commits the order at exactly that point.
func TestExpireHoldsRechecksPendingOrdersAfterLocking(t *testing.T) {
	e := newEnv(t, 2, nil)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	e.backdate(t, h.ID)

	booking.SetAfterLockExpired(e.svc, func() {
		conn, err := e.pool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()
		// In the real race the order's foreign-key checks ran before the sweeper
		// took its row lock; here they would wait on it, so skip them.
		_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `INSERT INTO orders (user_id, event_id, hold_id, total_cents, status, idempotency_key)
			VALUES ($1, $2, $3, 2500, 'pending_payment', 'k1')`, u, e.eventID, h.ID)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `RESET session_replication_role`)
		require.NoError(t, err)
	})

	n, err := e.svc.ExpireHolds(ctx, 100)
	require.NoError(t, err)
	assert.Zero(t, n, "the buyer may already have been charged")
	state, _, _, _ := e.seat(t, e.seats[0])
	assert.Equal(t, "held", state)
}

// If a charge succeeds for an order whose seats are gone, the buyer must get
// their money back, and the order must not block the reconciler.
func TestSucceededChargeForReleasedSeatsIsRefunded(t *testing.T) {
	e, m := newPayEnv(t, 2, mock.Config{LatencyMS: 300}, 50*time.Millisecond)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)
	require.Equal(t, "pending_payment", o.Status)

	// The state the sweeper race leaves behind.
	_, err = e.pool.Exec(ctx, `UPDATE event_seats SET state = 'available', hold_id = NULL, version = version + 1 WHERE hold_id = $1`, h.ID)
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE holds SET status = 'expired' WHERE id = $1`, h.ID)
	require.NoError(t, err)

	m.SetConfig(mock.Config{})
	_, err = e.svc.Reconcile(ctx, 0, 100) // settles the charge: the order is cancelled
	require.NoError(t, err)
	// The next pass refunds it. A negative age includes orders touched just now,
	// whatever the skew between this clock and the database's.
	r, err := e.svc.Reconcile(ctx, -time.Minute, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, r.Refunded)
	got, err := e.svc.GetOrder(ctx, u, o.ID)
	require.NoError(t, err)
	assert.Equal(t, "refunded", got.Status)
	e.noViolations(t)
}

// failFirstCharge fails the charge for one order with an unexpected error.
type failFirstCharge struct {
	payments.Client
	key string
}

func (f failFirstCharge) Charge(ctx context.Context, key string, amount int) (payments.Result, error) {
	if key == f.key {
		return payments.Result{}, errors.New("boom")
	}
	return f.Client.Charge(ctx, key, amount)
}

// One order that can't be settled must not stop the others.
func TestReconcileContinuesPastAFailingOrder(t *testing.T) {
	m := mock.New(mock.Config{LatencyMS: 300}, nil)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	inner := payments.NewHTTPClient(srv.URL, 50*time.Millisecond)
	pay := &failFirstCharge{Client: inner}
	e := newEnv(t, 2, pay)
	ctx := context.Background()

	var orders []uuid.UUID
	for i := range 2 {
		u := e.user(t)
		h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[i:i+1])
		require.NoError(t, err)
		o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
		require.NoError(t, err)
		require.Equal(t, "pending_payment", o.Status)
		orders = append(orders, o.ID)
	}
	pay.key = booking.ChargeKey(orders[0]) // the oldest, so it comes first

	m.SetConfig(mock.Config{})
	r, err := e.svc.Reconcile(ctx, 0, 100)
	assert.Error(t, err, "the failure is still reported")
	assert.Equal(t, 1, r.Confirmed)
}
