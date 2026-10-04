package booking

import "context"

// Violations counts rows that break the system's core promises. Every count must
// be zero; anything else means a seat was double-sold or money and seats disagree.
type Violations struct {
	SeatsWithMultipleValidTickets int `json:"seats_with_multiple_valid_tickets"`
	SoldSeatsWithoutConfirmedSale int `json:"sold_seats_without_confirmed_order"`
	ConfirmedOrdersTicketMismatch int `json:"confirmed_orders_ticket_mismatch"`
	ConfirmedOrdersNotPaidOnce    int `json:"confirmed_orders_not_paid_exactly_once"`
	HeldSeatsWithoutActiveHold    int `json:"held_seats_without_active_hold"`
}

// Total is the sum of all violation counts.
func (v Violations) Total() int {
	return v.SeatsWithMultipleValidTickets + v.SoldSeatsWithoutConfirmedSale +
		v.ConfirmedOrdersTicketMismatch + v.ConfirmedOrdersNotPaidOnce + v.HeldSeatsWithoutActiveHold
}

// invariantQueries are independent checks over the whole database. They are read
// only, so they can run against production at any time (Phase 8 alerts on them).
var invariantQueries = []struct {
	field func(*Violations) *int
	sql   string
}{
	{func(v *Violations) *int { return &v.SeatsWithMultipleValidTickets }, `
		SELECT count(*) FROM (SELECT event_seat_id FROM tickets WHERE status = 'valid'
		GROUP BY event_seat_id HAVING count(*) > 1) d`},
	{func(v *Violations) *int { return &v.SoldSeatsWithoutConfirmedSale }, `
		SELECT count(*) FROM event_seats es LEFT JOIN orders o ON o.id = es.order_id
		WHERE es.state = 'sold' AND (o.id IS NULL OR o.status <> 'confirmed')`},
	// Set-based: count valid tickets and sold seats per confirmed order with one
	// pass each, instead of two subqueries per order (22 s at 21,000 orders).
	{func(v *Violations) *int { return &v.ConfirmedOrdersTicketMismatch }, `
		SELECT count(*) FROM orders o
		LEFT JOIN (SELECT order_id, count(*) n FROM tickets WHERE status = 'valid' GROUP BY order_id) t ON t.order_id = o.id
		LEFT JOIN (SELECT order_id, count(*) n FROM event_seats WHERE state = 'sold' GROUP BY order_id) s ON s.order_id = o.id
		WHERE o.status = 'confirmed' AND coalesce(t.n, 0) <> coalesce(s.n, 0)`},
	{func(v *Violations) *int { return &v.ConfirmedOrdersNotPaidOnce }, `
		SELECT count(*) FROM orders o
		LEFT JOIN (SELECT order_id, count(*) n FROM payments WHERE kind = 'charge' AND status = 'succeeded' GROUP BY order_id) p
		  ON p.order_id = o.id
		WHERE o.status = 'confirmed' AND coalesce(p.n, 0) <> 1`},
	{func(v *Violations) *int { return &v.HeldSeatsWithoutActiveHold }, `
		SELECT count(*) FROM event_seats es LEFT JOIN holds h ON h.id = es.hold_id
		WHERE es.state = 'held' AND (h.id IS NULL OR h.status <> 'active')`},
}

// CheckInvariants runs every invariant query and reports the counts.
func (s *Service) CheckInvariants(ctx context.Context) (Violations, error) {
	var v Violations
	for _, iq := range invariantQueries {
		if err := s.pool.QueryRow(ctx, iq.sql).Scan(iq.field(&v)); err != nil {
			return v, err
		}
	}
	return v, nil
}

// ActiveHolds counts holds currently active (exported as a gauge).
func (s *Service) ActiveHolds(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE status = 'active'`).Scan(&n)
	return n, err
}
