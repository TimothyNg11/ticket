// Package booking is the heart of the system: holding seats, checking out,
// cancelling, and expiring abandoned holds.
//
// The rule every function follows: Postgres decides who owns a seat. Seats are
// claimed with row locks inside short transactions, constraints back every state
// change, and no transaction is ever held open across a network call to the
// payment provider.
package booking

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/apperr"
	"ticket/internal/auth"
	"ticket/internal/db/sqlc"
	"ticket/internal/payments"
)

// MaxSeatsPerHold is the most seats one hold may cover (spec: 8).
const MaxSeatsPerHold = 8

// Service implements booking operations.
type Service struct {
	pool     *pgxpool.Pool
	pay      payments.Client
	signer   *auth.TicketSigner
	holdTTL  time.Duration
	onChange func(eventID uuid.UUID, availableDelta int) // called after seat states change
}

// New returns a Service. Holds expire holdTTL after creation.
func New(pool *pgxpool.Pool, pay payments.Client, signer *auth.TicketSigner, holdTTL time.Duration) *Service {
	return &Service{pool: pool, pay: pay, signer: signer, holdTTL: holdTTL, onChange: func(uuid.UUID, int) {}}
}

// OnSeatsChanged registers fn to run after any commit that changes seat states for
// an event; availableDelta is the change in available seats. The cache uses it
// to invalidate seat maps and adjust availability counters. It must be fast and
// must not fail the caller: the commit has already happened.
func (s *Service) OnSeatsChanged(fn func(eventID uuid.UUID, availableDelta int)) { s.onChange = fn }

// Hold is a temporary claim on seats.
type Hold struct {
	ID        uuid.UUID
	EventID   uuid.UUID
	SeatIDs   []uuid.UUID
	ExpiresAt time.Time
	Status    string
}

// Ticket is one sold seat.
type Ticket struct {
	ID          uuid.UUID
	EventSeatID uuid.UUID
	Status      string
	QR          string
}

// Order is a purchase attempt and, once confirmed, its tickets.
type Order struct {
	ID         uuid.UUID
	EventID    uuid.UUID
	HoldID     uuid.UUID
	Status     string
	TotalCents int32
	CreatedAt  time.Time
	Tickets    []Ticket
}

var (
	errHoldNotFound  = apperr.NotFound("hold")
	errOrderNotFound = apperr.NotFound("order")
	errHoldInactive  = apperr.Conflict("HOLD_NOT_ACTIVE", "hold is no longer active")
	errInCheckout    = apperr.Conflict("CHECKOUT_IN_PROGRESS", "a checkout for this hold is already in progress")
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func toOrder(o sqlc.Order, tickets []sqlc.Ticket) Order {
	out := Order{ID: o.ID, EventID: o.EventID, HoldID: o.HoldID, Status: o.Status, TotalCents: o.TotalCents, CreatedAt: o.CreatedAt}
	for _, t := range tickets {
		if t.OrderID == o.ID {
			out.Tickets = append(out.Tickets, Ticket{ID: t.ID, EventSeatID: t.EventSeatID, Status: t.Status, QR: t.QrToken})
		}
	}
	return out
}

// loadOrder reads an order and its tickets.
func (s *Service) loadOrder(ctx context.Context, id uuid.UUID) (Order, error) {
	q := sqlc.New(s.pool)
	o, err := q.GetOrder(ctx, id)
	if err != nil {
		return Order{}, err
	}
	tickets, err := q.ListTicketsByOrders(ctx, []uuid.UUID{id})
	if err != nil {
		return Order{}, err
	}
	return toOrder(o, tickets), nil
}
