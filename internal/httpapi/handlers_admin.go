package httpapi

import (
	"context"

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
