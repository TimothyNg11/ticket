package httpapi

import (
	"context"

	"ticket/internal/db/sqlc"
	"ticket/internal/httpapi/gen"
)

// ListEvents returns published events, earliest start first, one page at a time.
func (s *Server) ListEvents(ctx context.Context, req gen.ListEventsRequestObject) (gen.ListEventsResponseObject, error) {
	limit := 20
	if req.Params.Limit != nil {
		limit = min(max(*req.Params.Limit, 1), 100)
	}
	cursor := ""
	if req.Params.Cursor != nil {
		cursor = *req.Params.Cursor
	}
	events, next, err := s.Inventory.ListEvents(ctx, cursor, limit)
	if err != nil {
		return nil, err
	}
	resp := gen.ListEvents200JSONResponse{Items: make([]gen.Event, 0, len(events))}
	for _, e := range events {
		resp.Items = append(resp.Items, toEvent(e))
	}
	if next != "" {
		resp.NextCursor = &next
	}
	return resp, nil
}

// GetEvent returns one published event.
func (s *Server) GetEvent(ctx context.Context, req gen.GetEventRequestObject) (gen.GetEventResponseObject, error) {
	e, err := s.Inventory.GetEvent(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetEvent200JSONResponse(toEvent(e)), nil
}

// GetSeatMap returns every seat of a published event with state and price.
func (s *Server) GetSeatMap(ctx context.Context, req gen.GetSeatMapRequestObject) (gen.GetSeatMapResponseObject, error) {
	sm, err := s.Inventory.SeatMap(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	resp := gen.GetSeatMap200JSONResponse{EventId: sm.EventID, Sections: make([]gen.SeatMapSection, 0, len(sm.Sections))}
	for _, sec := range sm.Sections {
		out := gen.SeatMapSection{Id: sec.ID, Name: sec.Name, Seats: make([]gen.SeatMapSeat, 0, len(sec.Seats))}
		for _, seat := range sec.Seats {
			out.Seats = append(out.Seats, gen.SeatMapSeat{
				EventSeatId: seat.EventSeatID, Row: seat.Row, Number: int(seat.Number),
				PriceCents: int(seat.PriceCents), State: seat.State,
			})
		}
		resp.Sections = append(resp.Sections, out)
	}
	return resp, nil
}

func toEvent(e sqlc.Event) gen.Event {
	return gen.Event{
		Id: e.ID, VenueId: e.VenueID, Name: e.Name,
		StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt, Status: e.Status,
	}
}
