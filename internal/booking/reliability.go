package booking

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"ticket/internal/db/sqlc"
	"ticket/internal/payments"
)

// emit writes a domain event to the outbox inside the caller's transaction, so
// the event exists if and only if the change it describes committed.
func emit(ctx context.Context, q *sqlc.Queries, eventType string, o sqlc.Order, extra map[string]any) error {
	payload := map[string]any{
		"event_id":    uuid.NewString(), // consumers deduplicate on this
		"order_id":    o.ID,
		"user_id":     o.UserID,
		"event":       o.EventID,
		"total_cents": o.TotalCents,
	}
	for k, v := range extra {
		payload[k] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return q.InsertOutbox(ctx, sqlc.InsertOutboxParams{AggregateID: o.ID, EventType: eventType, Payload: b})
}

// ReconcileResult counts what one reconciliation pass finished.
type ReconcileResult struct {
	Confirmed, Failed, StillPending, Refunded int
}

// Reconcile settles orders that checkout couldn't finish: orders stuck in
// pending_payment because the provider's answer was lost, and cancelled orders
// whose refund failed. It only touches orders untouched for olderThan, so it
// doesn't race a checkout that is still in progress.
//
// For a pending order it simply repeats the charge with the order's idempotency
// key. If the original charge went through, the provider returns that same charge;
// if it never arrived, this one is the first. Either way exactly one charge exists
// afterwards, and its definite answer settles the order.
func (s *Service) Reconcile(ctx context.Context, olderThan time.Duration, batch int) (ReconcileResult, error) {
	var res ReconcileResult
	q := sqlc.New(s.pool)
	cutoff := time.Now().Add(-olderThan)
	limit := int32(min(batch, 10_000)) //nolint:gosec // clamped

	pending, err := q.ListStalePendingOrders(ctx, sqlc.ListStalePendingOrdersParams{UpdatedAt: cutoff, Limit: limit})
	if err != nil {
		return res, err
	}
	for _, o := range pending {
		charge, err := s.pay.Charge(ctx, ChargeKey(o.ID), int(o.TotalCents))
		if errors.Is(err, payments.ErrUnknownOutcome) || errors.Is(err, payments.ErrCircuitOpen) {
			res.StillPending++ // try again next pass
			continue
		}
		if err != nil {
			return res, err
		}
		settled, err := s.ApplyChargeResult(ctx, o.ID, charge)
		if err != nil {
			return res, err
		}
		if settled.Status == "confirmed" {
			res.Confirmed++
		} else {
			res.Failed++
		}
	}

	cancelled, err := q.ListUnrefundedCancelledOrders(ctx, sqlc.ListUnrefundedCancelledOrdersParams{UpdatedAt: cutoff, Limit: limit})
	if err != nil {
		return res, err
	}
	for _, o := range cancelled {
		if err := s.RetryRefund(ctx, o.ID); err == nil {
			res.Refunded++
		}
	}
	return res, nil
}
