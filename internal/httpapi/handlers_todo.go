package httpapi

import (
	"context"

	"ticket/internal/apperr"
	"ticket/internal/httpapi/gen"
)

// Temporary stubs so the server satisfies the generated interface while the
// remaining handlers are built; removed as each handler lands.

var errTODO = apperr.Unavailable("not implemented")

func (s *Server) Register(context.Context, gen.RegisterRequestObject) (gen.RegisterResponseObject, error) {
	return nil, errTODO
}
func (s *Server) Login(context.Context, gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	return nil, errTODO
}
func (s *Server) RefreshTokens(context.Context, gen.RefreshTokensRequestObject) (gen.RefreshTokensResponseObject, error) {
	return nil, errTODO
}
func (s *Server) Logout(context.Context, gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	return nil, errTODO
}
func (s *Server) AdminCreateVenue(context.Context, gen.AdminCreateVenueRequestObject) (gen.AdminCreateVenueResponseObject, error) {
	return nil, errTODO
}
func (s *Server) AdminCreateEvent(context.Context, gen.AdminCreateEventRequestObject) (gen.AdminCreateEventResponseObject, error) {
	return nil, errTODO
}
func (s *Server) AdminPublishEvent(context.Context, gen.AdminPublishEventRequestObject) (gen.AdminPublishEventResponseObject, error) {
	return nil, errTODO
}
func (s *Server) ListEvents(context.Context, gen.ListEventsRequestObject) (gen.ListEventsResponseObject, error) {
	return nil, errTODO
}
func (s *Server) GetEvent(context.Context, gen.GetEventRequestObject) (gen.GetEventResponseObject, error) {
	return nil, errTODO
}
func (s *Server) GetSeatMap(context.Context, gen.GetSeatMapRequestObject) (gen.GetSeatMapResponseObject, error) {
	return nil, errTODO
}
