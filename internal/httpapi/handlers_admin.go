package httpapi

import (
	"context"

	"github.com/google/uuid"

	"ticket/internal/apperr"
	"ticket/internal/httpapi/gen"
	"ticket/internal/inventory"
)

// AdminCreateVenue creates a venue and its seat layout.
func (s *Server) AdminCreateVenue(ctx context.Context, req gen.AdminCreateVenueRequestObject) (gen.AdminCreateVenueResponseObject, error) {
	admin, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	spec := inventory.VenueSpec{Name: req.Body.Name, Timezone: req.Body.Timezone}
	for _, sec := range req.Body.Sections {
		ss := inventory.SectionSpec{Name: sec.Name}
		for _, r := range sec.Rows {
			ss.Rows = append(ss.Rows, inventory.RowSpec{Label: r.Label, SeatCount: r.SeatCount})
		}
		spec.Sections = append(spec.Sections, ss)
	}
	v, err := s.Inventory.CreateVenue(ctx, admin.UserID, spec)
	if err != nil {
		return nil, err
	}
	resp := gen.AdminCreateVenue201JSONResponse{Id: v.Venue.ID, Name: v.Venue.Name, Timezone: v.Venue.Timezone}
	for _, sec := range v.Sections {
		resp.Sections = append(resp.Sections, gen.VenueSection{Id: sec.Section.ID, Name: sec.Section.Name, SeatCount: sec.SeatCount})
	}
	return resp, nil
}

// AdminCreateEvent creates a draft event with per-section prices.
func (s *Server) AdminCreateEvent(ctx context.Context, req gen.AdminCreateEventRequestObject) (gen.AdminCreateEventResponseObject, error) {
	admin, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	prices := map[uuid.UUID]int32{}
	for _, p := range req.Body.SectionPrices {
		if _, dup := prices[p.SectionId]; dup {
			return nil, apperr.Validation("section " + p.SectionId.String() + " priced twice")
		}
		prices[p.SectionId] = int32(p.PriceCents) //nolint:gosec // validated: max=10000000
	}
	spec := inventory.EventSpec{
		VenueID: req.Body.VenueId, Name: req.Body.Name,
		StartsAt: req.Body.StartsAt, OnSaleAt: req.Body.OnSaleAt, SectionPrices: prices,
	}
	if q := req.Body.Queue; q != nil {
		spec.Queue.Enabled = q.Enabled != nil && *q.Enabled
		if q.BatchSize != nil {
			spec.Queue.Batch = int32(*q.BatchSize) //nolint:gosec // validated: max=100000
		}
		if q.IntervalSeconds != nil {
			spec.Queue.IntervalSeconds = int32(*q.IntervalSeconds) //nolint:gosec // validated: max=3600
		}
	}
	e, err := s.Inventory.CreateEvent(ctx, admin.UserID, spec)
	if err != nil {
		return nil, err
	}
	return gen.AdminCreateEvent201JSONResponse(toEvent(e)), nil
}

// AdminPublishEvent puts a draft event on sale.
func (s *Server) AdminPublishEvent(ctx context.Context, req gen.AdminPublishEventRequestObject) (gen.AdminPublishEventResponseObject, error) {
	admin, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.Inventory.PublishEvent(ctx, admin.UserID, req.Id)
	if err != nil {
		return nil, err
	}
	s.Cache.EventChanged(ctx, e.ID)
	return gen.AdminPublishEvent200JSONResponse(toEvent(e)), nil
}
