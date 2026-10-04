package httpapi

import (
	"context"
	"time"

	"ticket/internal/apperr"
	"ticket/internal/httpapi/gen"
)

// Healthz reports liveness: the process can serve HTTP. It checks no dependencies,
// so a database outage does not make Kubernetes restart healthy pods.
func (s *Server) Healthz(ctx context.Context, _ gen.HealthzRequestObject) (gen.HealthzResponseObject, error) {
	return gen.Healthz200JSONResponse{Status: "ok"}, nil
}

// Readyz reports readiness: not shutting down, and Postgres hasn't been
// unreachable for a sustained period (see DBHealth). Failing readiness takes the
// pod out of load balancing without restarting it.
func (s *Server) Readyz(ctx context.Context, _ gen.ReadyzRequestObject) (gen.ReadyzResponseObject, error) {
	if s.Draining != nil && s.Draining.Load() {
		return nil, apperr.Unavailable("shutting down")
	}
	if s.DBHealth != nil {
		if err := s.DBHealth.Ready(); err != nil {
			return nil, apperr.Unavailable("database unreachable")
		}
		return gen.Readyz200JSONResponse{Status: "ok"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		return nil, apperr.Unavailable("database unreachable")
	}
	return gen.Readyz200JSONResponse{Status: "ok"}, nil
}
