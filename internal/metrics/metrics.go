// Package metrics defines every Prometheus metric the system exports, in one
// place, so the dashboard and alert rules have a single list to match against.
//
// Naming follows Prometheus conventions: a ticket_ prefix, units in the name
// (_seconds, _total), and labels only with small, fixed value sets. Route labels
// use the chi route pattern (/v1/events/{id}/holds), never the raw path, which
// would create one time series per event id.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Latency buckets from 5 ms to 10 s: fine resolution where the SLOs live
// (tens to hundreds of ms) and a tail for timeouts.
var latencyBuckets = []float64{.005, .01, .025, .05, .1, .2, .3, .5, .75, 1, 2, 5, 10}

var (
	// HTTPRequests counts API requests by route, method and status code.
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_http_requests_total", Help: "API requests by route, method and status.",
	}, []string{"route", "method", "status"})

	// HTTPDuration is API latency by route and method.
	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ticket_http_request_duration_seconds", Help: "API request latency.", Buckets: latencyBuckets,
	}, []string{"route", "method"})

	// CacheLookups counts cache outcomes: hit, miss, stale (served while another
	// pod rebuilds), error (Redis unavailable, fell back to Postgres).
	CacheLookups = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_cache_lookups_total", Help: "Cache lookups by kind and result.",
	}, []string{"kind", "result"})

	// RateLimited counts requests rejected with 429, by bucket.
	RateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_rate_limited_total", Help: "Requests rejected by rate limits.",
	}, []string{"bucket"})

	// IdempotentReplays counts retried writes answered from the stored response.
	IdempotentReplays = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ticket_idempotent_replays_total", Help: "Writes answered from a stored idempotent response.",
	})

	// Holds counts hold outcomes: created, rejected (seat taken), released, expired.
	Holds = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_holds_total", Help: "Seat holds by outcome.",
	}, []string{"outcome"})

	// SeatsSold counts seats sold (net of nothing: cancellations are separate).
	SeatsSold = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ticket_seats_sold_total", Help: "Seats sold.",
	})

	// Orders counts orders reaching a final or notable state.
	Orders = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_orders_total", Help: "Orders by resulting status.",
	}, []string{"status"})

	// PaymentCalls counts provider calls by operation and outcome:
	// succeeded, declined, unknown, not_found, circuit_open, error.
	PaymentCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_payment_calls_total", Help: "Payment provider calls by operation and outcome.",
	}, []string{"op", "outcome"})

	// PaymentDuration is provider call latency per attempt.
	PaymentDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ticket_payment_call_duration_seconds", Help: "Payment provider call latency (per attempt).", Buckets: latencyBuckets,
	}, []string{"op"})

	// PaymentRetries counts retry attempts against the provider.
	PaymentRetries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ticket_payment_retries_total", Help: "Retried payment provider calls.",
	})

	// CircuitState is the payment circuit breaker: 0 closed, 1 open, 2 half-open.
	CircuitState = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ticket_payment_circuit_state", Help: "Payment circuit breaker state: 0 closed, 1 open, 2 half-open.",
	})

	// QueueLength is the number of buyers waiting in an event's waiting room.
	QueueLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ticket_queue_length", Help: "Buyers waiting in the queue.",
	}, []string{"event"})

	// Admitted counts buyers admitted from waiting rooms.
	Admitted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ticket_queue_admitted_total", Help: "Buyers admitted from waiting rooms.",
	})

	// OutboxLag is the age of the oldest unpublished outbox event.
	OutboxLag = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ticket_outbox_lag_seconds", Help: "Age of the oldest unpublished outbox event.",
	})

	// OutboxPublished counts events published by the relay.
	OutboxPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ticket_outbox_published_total", Help: "Outbox events published.",
	})

	// Notifications counts notifications sent (duplicates excluded).
	Notifications = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_notifications_total", Help: "Notifications sent by event type.",
	}, []string{"type"})

	// Reconciled counts orders settled by the reconciler.
	Reconciled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ticket_reconciled_total", Help: "Orders settled by the reconciler, by result.",
	}, []string{"result"})

	// InvariantViolations is the latest count of each core-guarantee violation.
	// Anything above zero pages someone.
	InvariantViolations = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ticket_invariant_violations", Help: "Rows violating a core guarantee (must be 0).",
	}, []string{"check"})

	// ActiveHolds is the number of holds currently active, sampled by the invariant job.
	ActiveHolds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ticket_active_holds", Help: "Holds currently active.",
	})
)

// Handler serves /metrics.
func Handler() http.Handler { return promhttp.Handler() }
