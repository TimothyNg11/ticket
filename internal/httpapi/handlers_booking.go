package httpapi

import (
	"context"

	"ticket/internal/booking"
	"ticket/internal/httpapi/gen"
	"ticket/internal/idempotency"
)

// CreateHold holds 1 to 8 seats for the caller.
func (s *Server) CreateHold(ctx context.Context, req gen.CreateHoldRequestObject) (gen.CreateHoldResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	h, err := s.Booking.CreateHold(ctx, user.UserID, req.Id, req.Body.SeatIds)
	if err != nil {
		return nil, err
	}
	return gen.CreateHold201JSONResponse{
		Id: h.ID, EventId: h.EventID, SeatIds: h.SeatIDs, ExpiresAt: h.ExpiresAt, Status: h.Status,
	}, nil
}

// ReleaseHold gives the caller's held seats back.
func (s *Server) ReleaseHold(ctx context.Context, req gen.ReleaseHoldRequestObject) (gen.ReleaseHoldResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Booking.ReleaseHold(ctx, user.UserID, req.Id); err != nil {
		return nil, err
	}
	return gen.ReleaseHold204Response{}, nil
}

// Checkout orders and pays for a hold.
func (s *Server) Checkout(ctx context.Context, req gen.CheckoutRequestObject) (gen.CheckoutResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	key, _ := idempotency.KeyFrom(ctx) // always present: the middleware rejects requests without one
	o, err := s.Booking.Checkout(ctx, user.UserID, req.Id, key)
	if err != nil {
		return nil, err
	}
	if o.Status == "pending_payment" {
		return gen.Checkout202JSONResponse(toOrder(o)), nil
	}
	return gen.Checkout201JSONResponse(toOrder(o)), nil
}

// ListOrders returns the caller's orders.
func (s *Server) ListOrders(ctx context.Context, req gen.ListOrdersRequestObject) (gen.ListOrdersResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	limit := 20
	if req.Params.Limit != nil {
		limit = min(max(*req.Params.Limit, 1), 50)
	}
	orders, err := s.Booking.ListOrders(ctx, user.UserID, limit)
	if err != nil {
		return nil, err
	}
	resp := gen.ListOrders200JSONResponse{Items: make([]gen.Order, 0, len(orders))}
	for _, o := range orders {
		resp.Items = append(resp.Items, toOrder(o))
	}
	return resp, nil
}

// GetOrder returns one of the caller's orders.
func (s *Server) GetOrder(ctx context.Context, req gen.GetOrderRequestObject) (gen.GetOrderResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.Booking.GetOrder(ctx, user.UserID, req.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetOrder200JSONResponse(toOrder(o)), nil
}

// CancelOrder cancels and refunds one of the caller's orders.
func (s *Server) CancelOrder(ctx context.Context, req gen.CancelOrderRequestObject) (gen.CancelOrderResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.Booking.CancelOrder(ctx, user.UserID, req.Id)
	if err != nil {
		return nil, err
	}
	return gen.CancelOrder200JSONResponse(toOrder(o)), nil
}

func toOrder(o booking.Order) gen.Order {
	out := gen.Order{
		Id: o.ID, EventId: o.EventID, HoldId: o.HoldID, Status: o.Status,
		TotalCents: int(o.TotalCents), CreatedAt: o.CreatedAt, Tickets: make([]gen.Ticket, 0, len(o.Tickets)),
	}
	for _, t := range o.Tickets {
		out.Tickets = append(out.Tickets, gen.Ticket{Id: t.ID, EventSeatId: t.EventSeatID, Status: t.Status, QrToken: t.QR})
	}
	return out
}
