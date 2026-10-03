package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ticket/internal/apperr"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
	"ticket/internal/metrics"
	"ticket/internal/payments"
)

// ChargeKey is the provider idempotency key for an order's charge. Deriving it from
// the order id means every retry of the same order, from checkout or from the
// reconciler, can only ever produce one charge.
func ChargeKey(orderID uuid.UUID) string { return "charge-" + orderID.String() }

func refundKey(orderID uuid.UUID) string { return "refund-" + orderID.String() }

var (
	errDeclined     = apperr.Unprocessable("PAYMENT_DECLINED", "the payment was declined; your seats are still held, so you can retry")
	errPaymentsDown = &apperr.Error{Status: 503, Code: "PAYMENT_UNAVAILABLE", Message: "payments are temporarily unavailable; your seats are still held, so retry shortly"}
)

// readier is implemented by payment clients that can report an outage up front
// (the circuit breaker in payments.Resilient).
type readier interface{ Ready() error }

// Checkout turns an active hold into an order and charges for it.
//
// It returns the order with status "confirmed" on success, or "pending_payment"
// when the provider's answer is unknown (timeout, provider error); the reconciler
// settles those later. A declined charge returns PAYMENT_DECLINED and leaves the
// hold active for a retry.
func (s *Service) Checkout(ctx context.Context, userID, holdID uuid.UUID, idemKey string) (Order, error) {
	// Fail fast while the payment provider is known to be down, before creating
	// an order that could only end up pending.
	if r, ok := s.pay.(readier); ok && r.Ready() != nil {
		return Order{}, errPaymentsDown
	}
	var order sqlc.Order
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		// A retry that lost its stored HTTP response still finds its order here.
		if o, err := q.GetOrderByKey(ctx, sqlc.GetOrderByKeyParams{UserID: userID, IdempotencyKey: idemKey}); err == nil {
			if o.HoldID != holdID {
				return apperr.Unprocessable("IDEMPOTENCY_KEY_REUSED", "this Idempotency-Key was already used for another hold")
			}
			order = o
			return nil
		} else if !isNoRows(err) {
			return err
		}

		h, err := q.GetHoldForUpdate(ctx, holdID)
		if isNoRows(err) || (err == nil && h.UserID != userID) {
			return errHoldNotFound
		}
		if err != nil {
			return err
		}
		if h.Status != "active" {
			return errHoldInactive
		}
		if !h.ExpiresAt.After(time.Now()) {
			return apperr.Conflict("HOLD_EXPIRED", "the hold expired; hold the seats again")
		}
		seats, err := q.ListHoldSeats(ctx, &holdID)
		if err != nil {
			return err
		}
		var total int32
		for _, st := range seats {
			total += st.PriceCents
		}
		order, err = q.CreateOrder(ctx, sqlc.CreateOrderParams{
			UserID: userID, EventID: h.EventID, HoldID: holdID, TotalCents: total, IdempotencyKey: idemKey,
		})
		if db.IsUniqueViolation(err) {
			return errInCheckout
		}
		return err
	})
	if err != nil {
		return Order{}, err
	}
	if order.Status != "pending_payment" {
		return s.loadOrder(ctx, order.ID)
	}

	// The transaction above has committed: no locks are held while we wait on the
	// network. The order row itself is what stops a second checkout of this hold.
	res, err := s.pay.Charge(ctx, ChargeKey(order.ID), int(order.TotalCents))
	if errors.Is(err, payments.ErrCircuitOpen) {
		// The breaker refused before anything was sent: nothing was charged, so the
		// order can safely fail and the user can retry once payments recover.
		if ferr := sqlc.New(s.pool).SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: order.ID, Status: "failed"}); ferr != nil {
			return Order{}, ferr
		}
		return Order{}, errPaymentsDown
	}
	if errors.Is(err, payments.ErrUnknownOutcome) {
		return s.loadOrder(ctx, order.ID)
	}
	if err != nil {
		return Order{}, err
	}
	o, err := s.ApplyChargeResult(ctx, order.ID, res)
	if err == nil && o.Status == "failed" {
		return Order{}, errDeclined
	}
	return o, err
}

// ApplyChargeResult settles a pending order with the provider's definite answer:
// on success it sells the seats and issues tickets, on decline it fails the order.
// Orders that are no longer pending are returned unchanged, so it is safe to call
// more than once (checkout and the reconciler may race).
func (s *Service) ApplyChargeResult(ctx context.Context, orderID uuid.UUID, res payments.Result) (Order, error) {
	var eventID uuid.UUID
	sold, soldSeats := false, 0
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		o, err := q.GetOrderForUpdate(ctx, orderID)
		if err != nil {
			return err
		}
		if o.Status != "pending_payment" {
			return nil
		}
		ref := res.Ref
		if err := q.CreatePayment(ctx, sqlc.CreatePaymentParams{
			OrderID: o.ID, Kind: "charge", ProviderRef: &ref, AmountCents: o.TotalCents,
			Status: string(res.Status), IdempotencyKey: ChargeKey(o.ID),
		}); err != nil {
			return err
		}
		if res.Status == payments.Declined {
			metrics.Orders.WithLabelValues("failed").Inc()
			return q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: o.ID, Status: "failed"})
		}

		seats, err := q.ListHoldSeats(ctx, &o.HoldID)
		if err != nil {
			return err
		}
		n, err := q.SellHoldSeats(ctx, sqlc.SellHoldSeatsParams{OrderID: o.ID, HoldID: o.HoldID})
		if err != nil {
			return err
		}
		if int(n) != len(seats) || n == 0 {
			// Should be impossible: the sweeper never expires a hold with a pending
			// order. Refuse rather than sell a partial order.
			return fmt.Errorf("booking: order %s: expected %d held seats, sold %d", o.ID, len(seats), n)
		}
		for _, st := range seats {
			id := uuid.New()
			if _, err := q.CreateTicket(ctx, sqlc.CreateTicketParams{
				ID: id, OrderID: o.ID, EventSeatID: st.ID, QrToken: s.signer.Sign(id, st.ID),
			}); err != nil {
				return err
			}
		}
		if err := q.SetHoldStatus(ctx, sqlc.SetHoldStatusParams{ID: o.HoldID, Status: "converted"}); err != nil {
			return err
		}
		eventID, sold, soldSeats = o.EventID, true, len(seats)
		if err := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: o.ID, Status: "confirmed"}); err != nil {
			return err
		}
		return emit(ctx, q, "order.confirmed", o, map[string]any{"seats": len(seats)})
	})
	if err != nil {
		return Order{}, err
	}
	if sold {
		metrics.Orders.WithLabelValues("confirmed").Inc()
		metrics.SeatsSold.Add(float64(soldSeats))
		s.onChange(eventID, 0) // held -> sold: availability unchanged, but the seat map changed
	}
	return s.loadOrder(ctx, orderID)
}
