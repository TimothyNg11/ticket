package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"

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
	events, next, err := s.Cache.ListEvents(ctx, cursor, limit)
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
	e, err := s.Cache.Event(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := toEvent(e)
	n, err := s.Cache.Available(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out.AvailableSeats = &n
	return gen.GetEvent200JSONResponse(out), nil
}

// GetSeatMap returns every seat of a published event with state and price. It
// writes the cache's pre-encoded, gzip-compressed body without touching JSON.
func (s *Server) GetSeatMap(ctx context.Context, req gen.GetSeatMapRequestObject) (gen.GetSeatMapResponseObject, error) {
	gz, err := s.Cache.SeatMapJSON(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return seatMapBody{gz: gz, acceptGzip: acceptsGzip(ctx)}, nil
}

// seatMapBody is a GetSeatMap response that's already encoded.
type seatMapBody struct {
	gz         []byte
	acceptGzip bool
}

// VisitGetSeatMapResponse implements gen.GetSeatMapResponseObject.
func (b seatMapBody) VisitGetSeatMapResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Vary", "Accept-Encoding")
	if b.acceptGzip {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(b.gz)
		return err
	}
	// Rare: a client that can't take gzip gets it decompressed.
	zr, err := gzip.NewReader(bytes.NewReader(b.gz))
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	// The data is our own cache entry; the cap just bounds a corrupted one.
	_, err = io.Copy(w, io.LimitReader(zr, 64<<20))
	return err
}

func toEvent(e sqlc.Event) gen.Event {
	q := e.QueueEnabled
	return gen.Event{
		Id: e.ID, VenueId: e.VenueID, Name: e.Name,
		StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt, Status: e.Status, QueueEnabled: &q,
	}
}
