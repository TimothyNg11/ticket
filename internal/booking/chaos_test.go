package booking_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/payments"
	"ticket/internal/payments/mock"
)

// TestPaymentChaosNeverLosesOrDoubleCharges is Phase 6's done-when. Sixty buyers
// check out against a provider that errors 30% of the time (half of those after
// charging), declines 10%, and is slower than the client's timeout 20% of the
// time. Then the reconciler runs until nothing is pending. Afterwards the
// provider's ledger and the orders table must agree exactly:
//
//   - every confirmed order was charged exactly once (a succeeded charge exists)
//   - no failed order was charged (no lost money)
//   - every succeeded charge belongs to a confirmed order (no orphaned charges)
//   - no order is left pending, and every database invariant holds
func TestPaymentChaosNeverLosesOrDoubleCharges(t *testing.T) {
	m := mock.New(mock.Config{ErrorRate: 0.3, DeclineRate: 0.1, LatencyMS: 2, SlowRate: 0.2, SlowMS: 80}, nil)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	pay := payments.NewResilient(payments.NewHTTPClient(srv.URL, 40*time.Millisecond),
		payments.RetryPolicy{Attempts: 3, Base: 5 * time.Millisecond, Max: 20 * time.Millisecond},
		payments.NewBreaker(1000, time.Second), nil)
	e := newEnv(t, 60, pay)
	e.svc = booking.New(e.pool, pay, auth.NewTicketSigner(signingKey), 10*time.Minute)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := e.user(t)
			h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[i:i+1])
			if !assert.NoError(t, err) {
				return
			}
			// A buyer whose payment is declined retries once with a new key, as a
			// real client would.
			for attempt := range 2 {
				_, err := e.svc.Checkout(ctx, u, h.ID, fmt.Sprintf("k%d", attempt))
				if code(err) != "PAYMENT_DECLINED" {
					return
				}
			}
		}()
	}
	wg.Wait()

	// The provider recovers; the reconciler settles everything left pending.
	m.SetConfig(mock.Config{})
	for range 10 {
		r, err := e.svc.Reconcile(ctx, 0, 1000)
		require.NoError(t, err)
		if r.StillPending == 0 {
			break
		}
	}

	ledger := m.Charged()
	rows, err := e.pool.Query(ctx, `SELECT id, status FROM orders`)
	require.NoError(t, err)
	orders := map[string]string{}
	for rows.Next() {
		var id uuid.UUID
		var status string
		require.NoError(t, rows.Scan(&id, &status))
		orders[booking.ChargeKey(id)] = status
	}
	require.NoError(t, rows.Err())

	counts := map[string]int{}
	for key, status := range orders {
		counts[status]++
		charged := ledger[key]
		switch status {
		case "confirmed":
			assert.Equal(t, "succeeded", charged, "confirmed order %s must have been charged", key)
		case "failed":
			assert.NotEqual(t, "succeeded", charged, "failed order %s was charged: lost money", key)
		default:
			t.Errorf("order %s left in status %s", key, status)
		}
	}
	for key, status := range ledger {
		if status == "succeeded" {
			assert.Equal(t, "confirmed", orders[key], "charge %s has no confirmed order: lost money", key)
		}
		assert.True(t, strings.HasPrefix(key, "charge-"))
	}
	e.noViolations(t)
	t.Logf("orders by status: %v; provider charges: %d", counts, len(ledger))
	assert.Positive(t, counts["confirmed"])
}

func TestCircuitOpenFailsFastWithoutOrders(t *testing.T) {
	m := mock.New(mock.Config{ErrorRate: 1}, nil)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	b := payments.NewBreaker(3, time.Minute)
	pay := payments.NewResilient(payments.NewHTTPClient(srv.URL, time.Second), payments.RetryPolicy{Attempts: 1}, b, nil)
	e := newEnv(t, 10, pay)
	ctx := context.Background()

	for i := range 3 { // three provider failures open the circuit
		u := e.user(t)
		h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[i:i+1])
		require.NoError(t, err)
		o, err := e.svc.Checkout(ctx, u, h.ID, "k")
		require.NoError(t, err)
		assert.Equal(t, "pending_payment", o.Status)
	}
	require.Equal(t, payments.Open, b.State())

	before := e.count(t, `SELECT count(*) FROM orders`)
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[5:6])
	require.NoError(t, err)
	start := time.Now()
	_, err = e.svc.Checkout(ctx, u, h.ID, "k")
	assert.Equal(t, "PAYMENT_UNAVAILABLE", code(err))
	assert.Less(t, time.Since(start), 100*time.Millisecond, "fails fast instead of waiting on a dead provider")
	assert.Equal(t, before, e.count(t, `SELECT count(*) FROM orders`), "no order created while the circuit is open")
	state, _, _, _ := e.seat(t, e.seats[5])
	assert.Equal(t, "held", state, "the hold survives so the buyer can retry")
}

func TestCheckoutWritesOutboxEventInSameTransaction(t *testing.T) {
	e, _ := newPayEnv(t, 2, mock.Config{}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k")
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND event_type = 'order.confirmed'`, o.ID))
	_, err = e.svc.CancelOrder(ctx, u, o.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND event_type = 'order.cancelled'`, o.ID))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND event_type = 'order.refunded'`, o.ID))
}
