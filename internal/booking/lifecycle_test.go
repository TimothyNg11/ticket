package booking_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// newPayEnv is newEnv wired to a mock provider over real HTTP.
func newPayEnv(t *testing.T, seats int, cfg mock.Config, timeout time.Duration) (*env, *mock.Server) {
	m := mock.New(cfg, nil)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return newEnv(t, seats, payments.NewHTTPClient(srv.URL, timeout)), m
}

func (e *env) backdate(t *testing.T, holdID uuid.UUID) {
	_, err := e.pool.Exec(context.Background(), `UPDATE holds SET expires_at = now() - interval '1 second' WHERE id = $1`, holdID)
	require.NoError(t, err)
}

func (e *env) noViolations(t *testing.T) {
	t.Helper()
	v, err := e.svc.CheckInvariants(context.Background())
	require.NoError(t, err)
	assert.Zero(t, v.Total(), "%+v", v)
}

// --- sweeper ---

func TestExpireHoldsReleasesSeats(t *testing.T) {
	e := newEnv(t, 4, nil)
	ctx := context.Background()
	h, err := e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[:2])
	require.NoError(t, err)
	fresh, err := e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[2:3])
	require.NoError(t, err)
	e.backdate(t, h.ID)

	n, err := e.svc.ExpireHolds(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the expired hold")
	state, hold, _, version := e.seat(t, e.seats[0])
	assert.Equal(t, "available", state)
	assert.Nil(t, hold)
	assert.Equal(t, 2, version)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM holds WHERE id = $1 AND status = 'expired'`, h.ID))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM holds WHERE id = $1 AND status = 'active'`, fresh.ID))
	e.noViolations(t)
}

func TestExpireHoldsSkipsPendingPayment(t *testing.T) {
	// The provider is slower than the client's timeout: outcome unknown, order pending.
	e, _ := newPayEnv(t, 2, mock.Config{LatencyMS: 300}, 50*time.Millisecond)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)
	assert.Equal(t, "pending_payment", o.Status)

	e.backdate(t, h.ID)
	n, err := e.svc.ExpireHolds(ctx, 100)
	require.NoError(t, err)
	assert.Zero(t, n, "the buyer may already have been charged")
	state, _, _, _ := e.seat(t, e.seats[0])
	assert.Equal(t, "held", state)
}

func TestConcurrentSweepersDontDoubleProcess(t *testing.T) {
	e := newEnv(t, 200, nil)
	ctx := context.Background()
	for i := range 200 {
		h, err := e.svc.CreateHold(ctx, e.user(t), e.eventID, e.seats[i:i+1])
		require.NoError(t, err)
		e.backdate(t, h.ID)
	}
	var total atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := e.svc.ExpireHolds(ctx, 25)
				if err != nil {
					errs <- err
					return
				}
				if n == 0 {
					return
				}
				total.Add(int64(n))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	assert.EqualValues(t, 200, total.Load())
	assert.Equal(t, 200, e.count(t, `SELECT count(*) FROM event_seats WHERE state = 'available'`))
}

// --- checkout ---

func TestCheckoutSuccess(t *testing.T) {
	e, m := newPayEnv(t, 3, mock.Config{}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:2])
	require.NoError(t, err)

	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)
	assert.Equal(t, "confirmed", o.Status)
	assert.Equal(t, 2*e.price, o.TotalCents)
	require.Len(t, o.Tickets, 2)
	signer := auth.NewTicketSigner(signingKey)
	for _, tk := range o.Tickets {
		tid, sid, err := signer.Verify(tk.QR)
		require.NoError(t, err)
		assert.Equal(t, tk.ID, tid)
		assert.Equal(t, tk.EventSeatID, sid)
	}
	for _, id := range e.seats[:2] {
		state, hold, order, _ := e.seat(t, id)
		assert.Equal(t, "sold", state)
		assert.Nil(t, hold)
		assert.Equal(t, o.ID, *order)
	}
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM holds WHERE id = $1 AND status = 'converted'`, h.ID))
	assert.Equal(t, 1, m.Charges())
	e.noViolations(t)

	again, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)
	assert.Equal(t, o.ID, again.ID, "same key returns the same order")
	assert.Equal(t, 1, m.Charges())
}

func TestCheckoutDeclinedThenRetry(t *testing.T) {
	e, m := newPayEnv(t, 2, mock.Config{DeclineRate: 1}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)

	_, err = e.svc.Checkout(ctx, u, h.ID, "k1")
	assert.Equal(t, "PAYMENT_DECLINED", code(err))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM orders WHERE status = 'failed'`))
	state, _, _, _ := e.seat(t, e.seats[0])
	assert.Equal(t, "held", state, "seats stay held for a retry")

	m.SetConfig(mock.Config{})
	o, err := e.svc.Checkout(ctx, u, h.ID, "k2")
	require.NoError(t, err)
	assert.Equal(t, "confirmed", o.Status)
	e.noViolations(t)
}

func TestCheckoutUnknownOutcomeThenSettle(t *testing.T) {
	e, _ := newPayEnv(t, 2, mock.Config{LatencyMS: 300}, 50*time.Millisecond)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)
	assert.Equal(t, "pending_payment", o.Status)

	// Later, the provider's answer arrives (Phase 6's reconciler does this).
	settled, err := e.svc.ApplyChargeResult(ctx, o.ID, payments.Result{Status: payments.Succeeded, Ref: "ch_1"})
	require.NoError(t, err)
	assert.Equal(t, "confirmed", settled.Status)
	again, err := e.svc.ApplyChargeResult(ctx, o.ID, payments.Result{Status: payments.Succeeded, Ref: "ch_1"})
	require.NoError(t, err)
	assert.Len(t, again.Tickets, 1, "applying twice is harmless")
	e.noViolations(t)
}

func TestCheckoutRejections(t *testing.T) {
	e, _ := newPayEnv(t, 3, mock.Config{}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)

	_, err = e.svc.Checkout(ctx, e.user(t), h.ID, "k")
	assert.Equal(t, "NOT_FOUND", code(err), "not the owner")
	e.backdate(t, h.ID)
	_, err = e.svc.Checkout(ctx, u, h.ID, "k")
	assert.Equal(t, "HOLD_EXPIRED", code(err))
	_, err = e.svc.ExpireHolds(ctx, 10)
	require.NoError(t, err)
	_, err = e.svc.Checkout(ctx, u, h.ID, "k")
	assert.Equal(t, "HOLD_NOT_ACTIVE", code(err))
}

func TestDoubleCheckoutParallel(t *testing.T) {
	e, m := newPayEnv(t, 2, mock.Config{LatencyMS: 20}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:2])
	require.NoError(t, err)

	var wg sync.WaitGroup
	var confirmed, inProgress atomic.Int64
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := e.svc.Checkout(ctx, u, h.ID, fmt.Sprintf("k%d", i))
			switch {
			case err == nil && o.Status == "confirmed":
				confirmed.Add(1)
			case code(err) == "CHECKOUT_IN_PROGRESS" || code(err) == "HOLD_NOT_ACTIVE":
				inProgress.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, confirmed.Load())
	assert.EqualValues(t, 19, inProgress.Load())
	assert.Equal(t, 1, m.Charges())
	e.noViolations(t)
}

// --- orders ---

func TestOrdersAndCancel(t *testing.T) {
	e, _ := newPayEnv(t, 3, mock.Config{}, time.Second)
	ctx := context.Background()
	u, other := e.user(t), e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:2])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)

	_, err = e.svc.GetOrder(ctx, other, o.ID)
	assert.Equal(t, "NOT_FOUND", code(err))
	got, err := e.svc.GetOrder(ctx, u, o.ID)
	require.NoError(t, err)
	assert.Equal(t, o, got)
	list, err := e.svc.ListOrders(ctx, u, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)

	_, err = e.svc.CancelOrder(ctx, other, o.ID)
	assert.Equal(t, "NOT_FOUND", code(err))
	c, err := e.svc.CancelOrder(ctx, u, o.ID)
	require.NoError(t, err)
	assert.Equal(t, "refunded", c.Status)
	for _, tk := range c.Tickets {
		assert.Equal(t, "void", tk.Status)
	}
	state, _, order, _ := e.seat(t, e.seats[0])
	assert.Equal(t, "available", state)
	assert.Nil(t, order)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM payments WHERE order_id = $1 AND kind = 'refund'`, o.ID))
	_, err = e.svc.CancelOrder(ctx, u, o.ID)
	assert.Equal(t, "NOT_CANCELLABLE", code(err))

	// The released seat can be sold again, and gets a new valid ticket.
	buyer := e.user(t)
	h2, err := e.svc.CreateHold(ctx, buyer, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o2, err := e.svc.Checkout(ctx, buyer, h2.ID, "k1")
	require.NoError(t, err)
	assert.Equal(t, "confirmed", o2.Status)
	assert.Equal(t, 2, e.count(t, `SELECT count(*) FROM tickets WHERE event_seat_id = $1`, e.seats[0]))
	e.noViolations(t)
}

func TestCancelRefundFailsStaysCancelled(t *testing.T) {
	e, m := newPayEnv(t, 2, mock.Config{}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)

	m.SetConfig(mock.Config{ErrorRate: 1})
	c, err := e.svc.CancelOrder(ctx, u, o.ID)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", c.Status)

	m.SetConfig(mock.Config{})
	require.NoError(t, e.svc.RetryRefund(ctx, o.ID))
	got, err := e.svc.GetOrder(ctx, u, o.ID)
	require.NoError(t, err)
	assert.Equal(t, "refunded", got.Status)
}

func TestCancelAfterEventStarts(t *testing.T) {
	e, _ := newPayEnv(t, 2, mock.Config{}, time.Second)
	ctx := context.Background()
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[:1])
	require.NoError(t, err)
	o, err := e.svc.Checkout(ctx, u, h.ID, "k1")
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE events SET on_sale_at = now() - interval '2 days', starts_at = now() - interval '1 minute' WHERE id = $1`, e.eventID)
	require.NoError(t, err)
	_, err = e.svc.CancelOrder(ctx, u, o.ID)
	assert.Equal(t, "EVENT_STARTED", code(err))
}

// --- the whole thing under contention ---

// TestFlashSaleEndToEnd races 200 buyers for 10 seats through hold → checkout,
// then some cancel and others buy the released seats. No invariant may break.
func TestFlashSaleEndToEnd(t *testing.T) {
	e, _ := newPayEnv(t, 10, mock.Config{LatencyMS: 5}, time.Second)
	ctx := context.Background()
	users := make([]uuid.UUID, 200)
	for i := range users {
		users[i] = e.user(t)
	}

	type sale struct {
		user  uuid.UUID
		order booking.Order
	}
	race := func(buyers []uuid.UUID) []sale {
		var mu sync.Mutex
		var orders []sale
		var wg sync.WaitGroup
		for _, u := range buyers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				seat := e.seats[rand.IntN(len(e.seats))]
				h, err := e.svc.CreateHold(ctx, u, e.eventID, []uuid.UUID{seat})
				if err != nil {
					return
				}
				o, err := e.svc.Checkout(ctx, u, h.ID, "k-"+u.String())
				if err == nil && o.Status == "confirmed" {
					mu.Lock()
					orders = append(orders, sale{u, o})
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return orders
	}

	first := race(users[:100])
	assert.LessOrEqual(t, len(first), 10)
	assert.NotEmpty(t, first)
	e.noViolations(t)

	for _, s := range first[:len(first)/2] {
		_, err := e.svc.CancelOrder(ctx, s.user, s.order.ID)
		require.NoError(t, err)
	}
	race(users[100:])
	e.noViolations(t)
	assert.Equal(t, 0, e.count(t, `SELECT count(*) FROM event_seats WHERE state = 'held'`), "every hold either converted or is still owned")
}

// TestInvariantsDetectEveryViolation plants each kind of violation directly in
// the database and checks the checker counts it. It guards the invariant SQL
// itself, which was rewritten for speed in Phase 9 (a correlated subquery per
// order took 22 s at 21,000 orders).
func TestInvariantsDetectEveryViolation(t *testing.T) {
	e, _ := newPayEnv(t, 6, mock.Config{}, time.Second)
	ctx := context.Background()
	buy := func(seat uuid.UUID) booking.Order {
		u := e.user(t)
		h, err := e.svc.CreateHold(ctx, u, e.eventID, []uuid.UUID{seat})
		require.NoError(t, err)
		o, err := e.svc.Checkout(ctx, u, h.ID, "k")
		require.NoError(t, err)
		return o
	}
	o1, o2, o3 := buy(e.seats[0]), buy(e.seats[1]), buy(e.seats[2])
	e.noViolations(t)
	exec := func(sql string, args ...any) {
		_, err := e.pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}

	// 1. A second valid ticket for a seat: drop the guard index to plant it.
	exec(`DROP INDEX tickets_one_valid_per_seat`)
	exec(`INSERT INTO tickets (order_id, event_seat_id, qr_token) SELECT order_id, event_seat_id, 'dup' FROM tickets WHERE order_id = $1`, o1.ID)
	// 2. A sold seat whose order isn't confirmed.
	exec(`UPDATE orders SET status = 'failed' WHERE id = $1`, o2.ID)
	// 3. A confirmed order paid twice.
	exec(`INSERT INTO payments (order_id, kind, amount_cents, status, idempotency_key) VALUES ($1, 'charge', 1, 'succeeded', 'x2')`, o3.ID)
	// 4. A held seat with no active hold.
	exec(`UPDATE holds SET status = 'expired' WHERE id IN (SELECT hold_id FROM orders WHERE id = $1)`, o3.ID)
	u := e.user(t)
	h, err := e.svc.CreateHold(ctx, u, e.eventID, e.seats[3:4])
	require.NoError(t, err)
	exec(`UPDATE holds SET status = 'released' WHERE id = $1`, h.ID)

	v, err := e.svc.CheckInvariants(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, v.SeatsWithMultipleValidTickets)
	assert.Equal(t, 1, v.SoldSeatsWithoutConfirmedSale)
	assert.Equal(t, 1, v.ConfirmedOrdersTicketMismatch, "o1: 2 valid tickets for 1 sold seat")
	assert.Equal(t, 1, v.ConfirmedOrdersNotPaidOnce)
	assert.Equal(t, 1, v.HeldSeatsWithoutActiveHold)
}
