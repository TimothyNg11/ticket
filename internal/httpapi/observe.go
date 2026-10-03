package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"ticket/internal/metrics"
	"ticket/internal/observability"
)

// observe records one access-log line, the request metrics, and the trace span
// name for every request. It runs outermost (after RequestID), and reads the
// matched route pattern after the router has run, so metrics and spans are
// labelled /v1/events/{id}/holds rather than with raw ids.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := middleware.GetReqID(r.Context())
		w.Header().Set("X-Request-Id", reqID) // so clients can quote it in bug reports
		r = r.WithContext(observability.WithRequestID(r.Context(), reqID))
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		elapsed := time.Since(start)
		metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(status)).Inc()
		metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())

		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))

		if route == "/healthz" || route == "/readyz" || route == "/metrics" {
			return // probes and scrapes would drown the access log
		}
		attrs := []any{"method", r.Method, "route", route, "status", status,
			"duration_ms", float64(elapsed.Microseconds()) / 1000, "bytes", ww.BytesWritten()}
		if c, ok := userFrom(r.Context()); ok {
			attrs = append(attrs, "user_id", c.UserID)
		}
		s.Log.InfoContext(r.Context(), "request", attrs...)
	})
}
