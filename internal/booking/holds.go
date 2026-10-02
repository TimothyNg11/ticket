package booking

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticket/internal/apperr"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
)

// CreateHold claims seats for userID for holdTTL. It either holds every requested
// seat or none of them.
func (s *Service) CreateHold(ctx context.Context, userID, eventID uuid.UUID, seatIDs []uuid.UUID) (Hold, error) {
	var out Hold
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		ev, err := q.GetEvent(ctx, eventID)
		if isNoRows(err) || (err == nil && ev.Status == "draft") {
			return apperr.NotFound("event")
		}
		if err != nil {
			return err
		}
		if ev.Status != "on_sale" || ev.OnSaleAt.After(time.Now()) {
			return apperr.Unprocessable("NOT_ON_SALE", "this event is not on sale")
		}

		// The row locks taken here are what make double-holding impossible: two
		// transactions can never both lock the same available seat.
		locked, err := q.LockAvailableSeats(ctx, sqlc.LockAvailableSeatsParams{EventID: eventID, SeatIds: seatIDs})
		if err != nil {
			return err
		}
		if len(locked) < len(seatIDs) {
			return seatsUnavailable(seatIDs, locked)
		}

		h, err := q.CreateHold(ctx, sqlc.CreateHoldParams{EventID: eventID, UserID: userID, ExpiresAt: time.Now().Add(s.holdTTL)})
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("HOLD_EXISTS", "you already have an active hold for this event; release or check it out first")
		}
		if err != nil {
			return err
		}
		if _, err := q.HoldSeats(ctx, sqlc.HoldSeatsParams{HoldID: h.ID, SeatIds: locked}); err != nil {
			return err
		}
		out = Hold{ID: h.ID, EventID: eventID, SeatIDs: locked, ExpiresAt: h.ExpiresAt, Status: h.Status}
		return nil
	})
	if err == nil {
		s.onChange(eventID, -len(out.SeatIDs))
	}
	return out, err
}

func seatsUnavailable(requested, locked []uuid.UUID) error {
	got := map[uuid.UUID]bool{}
	for _, id := range locked {
		got[id] = true
	}
	var missing []string
	for _, id := range requested {
		if !got[id] {
			missing = append(missing, id.String())
		}
	}
	return apperr.Conflict("SEAT_UNAVAILABLE", "seats unavailable: "+strings.Join(missing, ", "))
}

// ReleaseHold gives a hold's seats back. Only the owner may release it, and not
// while a payment for it is in flight.
func (s *Service) ReleaseHold(ctx context.Context, userID, holdID uuid.UUID) error {
	var eventID uuid.UUID
	var released int64
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		h, err := q.GetHoldForUpdate(ctx, holdID)
		if isNoRows(err) || (err == nil && h.UserID != userID) {
			return errHoldNotFound // not 403: don't confirm that someone else's hold exists
		}
		if err != nil {
			return err
		}
		if h.Status != "active" {
			return errHoldInactive
		}
		pending, err := q.HasPendingOrderForHold(ctx, holdID)
		if err != nil {
			return err
		}
		if pending {
			return errInCheckout
		}
		if released, err = q.ReleaseHoldSeats(ctx, &holdID); err != nil {
			return err
		}
		eventID = h.EventID
		return q.SetHoldStatus(ctx, sqlc.SetHoldStatusParams{ID: holdID, Status: "released"})
	})
	if err == nil {
		s.onChange(eventID, int(released))
	}
	return err
}

// ExpireHolds releases up to batch holds whose time ran out and returns how many
// it expired. Safe to run from many replicas at once: SKIP LOCKED hands each
// replica a different set of holds.
func (s *Service) ExpireHolds(ctx context.Context, batch int) (int, error) {
	var n int
	freed := map[uuid.UUID]int{} // seats released per event
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		ids, err := q.LockExpiredHolds(ctx, int32(min(batch, 10_000))) //nolint:gosec // clamped
		if err != nil || len(ids) == 0 {
			return err
		}
		events, err := q.ReleaseSeatsOfHolds(ctx, ids)
		if err != nil {
			return err
		}
		for _, e := range events {
			freed[e]++
		}
		n = len(ids)
		return q.ExpireHolds(ctx, ids)
	})
	if err != nil {
		return 0, err
	}
	for id, seats := range freed {
		s.onChange(id, seats)
	}
	return n, nil
}
