package booking

import (
	"context"
	"time"

	"github.com/google/uuid"

	"ticket/internal/apperr"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
	"ticket/internal/metrics"
	"ticket/internal/payments"
)

// GetOrder returns one of the user's orders. Other users' orders are reported as
// not found, so order ids can't be probed.
func (s *Service) GetOrder(ctx context.Context, userID, orderID uuid.UUID) (Order, error) {
	o, err := s.loadOrder(ctx, orderID)
	if isNoRows(err) {
		return Order{}, errOrderNotFound
	}
	if err != nil {
		return Order{}, err
	}
	owner, err := sqlc.New(s.pool).GetOrder(ctx, orderID)
	if err != nil {
		return Order{}, err
	}
	if owner.UserID != userID {
		return Order{}, errOrderNotFound
	}
	return o, nil
}

// ListOrders returns the user's most recent orders, newest first.
func (s *Service) ListOrders(ctx context.Context, userID uuid.UUID, limit int) ([]Order, error) {
	q := sqlc.New(s.pool)
	orders, err := q.ListOrdersByUser(ctx, sqlc.ListOrdersByUserParams{UserID: userID, Limit: int32(min(limit, 1000))}) //nolint:gosec // clamped
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(orders))
	for i, o := range orders {
		ids[i] = o.ID
	}
	tickets, err := q.ListTicketsByOrders(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Order, len(orders))
	for i, o := range orders {
		out[i] = toOrder(o, tickets)
	}
	return out, nil
}

// CancelOrder cancels a confirmed order before the event starts: tickets are
// voided, seats go back on sale, and the charge is refunded. If the refund call
// fails, the order stays "cancelled" (seats already released) and RetryRefund
// finishes it later.
func (s *Service) CancelOrder(ctx context.Context, userID, orderID uuid.UUID) (Order, error) {
	var o sqlc.Order
	var released int64
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		var err error
		o, err = q.GetOrderForUpdate(ctx, orderID)
		if isNoRows(err) || (err == nil && o.UserID != userID) {
			return errOrderNotFound
		}
		if err != nil {
			return err
		}
		if o.Status != "confirmed" {
			return apperr.Conflict("NOT_CANCELLABLE", "only confirmed orders can be cancelled")
		}
		ev, err := q.GetEvent(ctx, o.EventID)
		if err != nil {
			return err
		}
		if !ev.StartsAt.After(time.Now()) {
			return apperr.Unprocessable("EVENT_STARTED", "orders can't be cancelled after the event starts")
		}
		if err := q.VoidOrderTickets(ctx, o.ID); err != nil {
			return err
		}
		if released, err = q.ReleaseOrderSeats(ctx, &o.ID); err != nil {
			return err
		}
		if err := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: o.ID, Status: "cancelled"}); err != nil {
			return err
		}
		return emit(ctx, q, "order.cancelled", o, nil)
	})
	if err != nil {
		return Order{}, err
	}
	metrics.Orders.WithLabelValues("cancelled").Inc()
	s.onChange(o.EventID, int(released))
	// The refund happens after commit, outside the transaction. A failure here is
	// not the user's problem: the cancellation stands and the refund is retried.
	_ = s.RetryRefund(ctx, o.ID)
	return s.loadOrder(ctx, o.ID)
}

// RetryRefund refunds a cancelled order and marks it refunded. Safe to repeat:
// the refund idempotency key is derived from the order id.
func (s *Service) RetryRefund(ctx context.Context, orderID uuid.UUID) error {
	q := sqlc.New(s.pool)
	o, err := q.GetOrder(ctx, orderID)
	if err != nil || o.Status != "cancelled" {
		return err
	}
	ref, err := q.GetChargeRef(ctx, o.ID)
	if err != nil {
		return err
	}
	chargeRef := ""
	if ref != nil {
		chargeRef = *ref
	}
	res, err := s.pay.Refund(ctx, refundKey(o.ID), chargeRef, int(o.TotalCents))
	if err != nil {
		return err
	}
	if res.Status != payments.Succeeded {
		return apperr.Unavailable("refund not completed")
	}
	return db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		cur, err := q.GetOrderForUpdate(ctx, o.ID)
		if err != nil || cur.Status != "cancelled" {
			return err
		}
		r := res.Ref
		if err := q.CreatePayment(ctx, sqlc.CreatePaymentParams{
			OrderID: o.ID, Kind: "refund", ProviderRef: &r, AmountCents: o.TotalCents,
			Status: string(res.Status), IdempotencyKey: refundKey(o.ID),
		}); err != nil {
			return err
		}
		if err := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: o.ID, Status: "refunded"}); err != nil {
			return err
		}
		return emit(ctx, q, "order.refunded", o, nil)
	})
}
