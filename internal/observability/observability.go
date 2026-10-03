// Package observability sets up structured logging and tracing for every binary.
package observability

import (
	"context"
	"log/slog"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

type requestIDKey struct{}

// WithRequestID stores the request id so every log line can carry it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request id stored in ctx.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// contextHandler adds request_id and trace_id from the context to every record,
// so any log line written while handling a request can be tied back to it and
// to its trace, without each call site passing ids around.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(a)}
}

func (h contextHandler) WithGroup(n string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(n)}
}

// NewLogger returns a JSON logger tagged with the service name. Use the
// *Context variants (InfoContext, ...) so request and trace ids are attached.
func NewLogger(service string) *slog.Logger {
	return slog.New(contextHandler{slog.NewJSONHandler(os.Stdout, nil)}).With("service", service)
}

// InitTracing installs an OpenTelemetry tracer provider that exports spans over
// OTLP/HTTP to OTEL_EXPORTER_OTLP_ENDPOINT (e.g. http://jaeger:4318). With no
// endpoint configured, tracing stays a no-op. OTEL_TRACES_SAMPLER_ARG sets the
// sampled fraction (default 1.0; lower it under heavy load).
func InitTracing(ctx context.Context, service string) (shutdown func(context.Context) error, err error) {
	// Propagate W3C trace context on outgoing calls even when not exporting, so
	// a caller's trace continues through us.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	ratio := 1.0
	if v, err := strconv.ParseFloat(os.Getenv("OTEL_TRACES_SAMPLER_ARG"), 64); err == nil {
		ratio = v
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(service))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// RegisterPoolMetrics exports pgxpool statistics: how many connections are in
// use, idle, and how long requests wait to get one. Pool exhaustion shows up
// here before it shows up as latency.
func RegisterPoolMetrics(pool *pgxpool.Pool) {
	gauge := func(name, help string, f func(*pgxpool.Stat) float64) {
		prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help},
			func() float64 { return f(pool.Stat()) }))
	}
	gauge("ticket_db_pool_acquired_conns", "Connections in use.", func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) })
	gauge("ticket_db_pool_idle_conns", "Idle connections.", func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) })
	gauge("ticket_db_pool_max_conns", "Pool size limit.", func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) })
	prometheus.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "ticket_db_pool_acquire_wait_seconds_total", Help: "Total time spent waiting for a connection.",
	}, func() float64 { return pool.Stat().AcquireDuration().Seconds() }))
	prometheus.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "ticket_db_pool_empty_acquires_total", Help: "Acquires that had to wait because no connection was idle.",
	}, func() float64 { return float64(pool.Stat().EmptyAcquireCount()) }))
}
