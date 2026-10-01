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

// Readyz reports readiness: Postgres answers within a second. Failing readiness
// takes the pod out of load balancing without restarting it.
func (s *Server) Readyz(ctx context.Context, _ gen.ReadyzRequestObject) (gen.ReadyzResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		return nil, apperr.Unavailable("database unreachable")
	}
	return gen.Readyz200JSONResponse{Status: "ok"}, nil
}
