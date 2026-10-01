package httpapi

import (
	"context"

	"ticket/internal/apperr"
	"ticket/internal/httpapi/gen"
)

// Temporary stubs so the server satisfies the generated interface while the
// remaining handlers are built; removed as each handler lands.

var errTODO = apperr.Unavailable("not implemented")

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
