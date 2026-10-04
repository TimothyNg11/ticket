package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"ticket/internal/apperr"
	"ticket/internal/httpapi/gen"
	"ticket/internal/waitingroom"
)

var errAdmissionRequired = &apperr.Error{Status: http.StatusForbidden, Code: "ADMISSION_REQUIRED",
	Message: "this event has a waiting room; join the queue and send your admission token in the Admission-Token header"}

// JoinQueue puts the caller in an event's waiting room.
func (s *Server) JoinQueue(ctx context.Context, req gen.JoinQueueRequestObject) (gen.JoinQueueResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ev, err := s.Cache.Event(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if !ev.QueueEnabled {
		return nil, apperr.Unprocessable("QUEUE_NOT_ENABLED", "this event has no waiting room; hold seats directly")
	}
	p, err := s.Room.Join(ctx, ev.ID, user.UserID, rateOf(ev.AdmitBatch, ev.AdmitIntervalSeconds))
	if err != nil {
		return nil, err
	}
	out := toQueueStatus(p, rateOf(ev.AdmitBatch, ev.AdmitIntervalSeconds))
	s.markSoldOut(ctx, ev.ID, &out)
	return gen.JoinQueue200JSONResponse(out), nil
}

// GetQueueStatus reports the caller's place in line, or their admission token.
func (s *Server) GetQueueStatus(ctx context.Context, req gen.GetQueueStatusRequestObject) (gen.GetQueueStatusResponseObject, error) {
	user, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var rate waitingroom.Rate
	pass, p, err := s.Room.Status(ctx, req.Token, func(eventID uuid.UUID) waitingroom.Rate {
		ev, err := s.Cache.Event(ctx, eventID)
		if err != nil {
			return waitingroom.Rate{}
		}
		rate = rateOf(ev.AdmitBatch, ev.AdmitIntervalSeconds)
		return rate
	})
	switch {
	case errors.Is(err, waitingroom.ErrNotQueued):
		return nil, apperr.NotFound("queue position (admission may have expired; join again)")
	case err != nil && pass.UserID == uuid.Nil:
		return nil, apperr.Unauthenticated("invalid queue token")
	case err != nil:
		return nil, err
	case pass.UserID != user.UserID:
		// Queue tokens are bound to the account that joined.
		return nil, apperr.NotFound("queue position")
	}
	out := toQueueStatus(p, rate)
	s.markSoldOut(ctx, pass.EventID, &out)
	return gen.GetQueueStatus200JSONResponse(out), nil
}

// markSoldOut tells waiting buyers when nothing is left, so they stop polling
// instead of waiting for an admission that can't help them. It reads the cached
// availability counter, so it costs one Redis GET.
func (s *Server) markSoldOut(ctx context.Context, eventID uuid.UUID, out *gen.QueueStatus) {
	if n, err := s.Cache.Available(ctx, eventID); err == nil && n == 0 {
		soldOut := true
		out.SoldOut = &soldOut
	}
}

// checkAdmission enforces the waiting room on holds: if the event has a queue,
// the caller must present an admission pass issued to them for this event.
func (s *Server) checkAdmission(ctx context.Context, userID, eventID uuid.UUID, token *string) error {
	ev, err := s.Cache.Event(ctx, eventID)
	if err != nil {
		return err
	}
	if !ev.QueueEnabled {
		return nil
	}
	if token == nil || s.Room.CheckAdmission(*token, userID, eventID) != nil {
		return errAdmissionRequired
	}
	return nil
}

func rateOf(batch, intervalSeconds int32) waitingroom.Rate {
	return waitingroom.Rate{Batch: int(batch), Interval: secs(intervalSeconds)}
}

func toQueueStatus(p waitingroom.Position, rate waitingroom.Rate) gen.QueueStatus {
	out := gen.QueueStatus{QueueToken: p.QueueToken, Admitted: p.Admitted}
	if p.Admitted {
		out.AdmissionToken = &p.AdmissionToken
		out.AdmissionExpiresAt = &p.AdmissionExpires
		return out
	}
	pos, wait, poll := p.Position, int(p.EstimatedWait.Seconds()), int(rate.PollAfter(p.Position).Seconds())
	out.Position, out.EstimatedWaitSeconds, out.PollAfterSeconds = &pos, &wait, &poll
	return out
}

func secs(n int32) time.Duration { return time.Duration(n) * time.Second }
