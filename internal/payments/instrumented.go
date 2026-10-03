package payments

import (
	"context"
	"errors"
	"time"

	"ticket/internal/metrics"
)

// Instrumented records the outcome and latency of every provider call.
type Instrumented struct{ next Client }

// NewInstrumented wraps next with metrics.
func NewInstrumented(next Client) *Instrumented { return &Instrumented{next: next} }

// Charge implements Client.
func (i *Instrumented) Charge(ctx context.Context, key string, amount int) (Result, error) {
	return record("charge", func() (Result, error) { return i.next.Charge(ctx, key, amount) })
}

// Refund implements Client.
func (i *Instrumented) Refund(ctx context.Context, key, ref string, amount int) (Result, error) {
	return record("refund", func() (Result, error) { return i.next.Refund(ctx, key, ref, amount) })
}

// GetCharge implements Client.
func (i *Instrumented) GetCharge(ctx context.Context, key string) (Result, error) {
	return record("get_charge", func() (Result, error) { return i.next.GetCharge(ctx, key) })
}

func record(op string, call func() (Result, error)) (Result, error) {
	start := time.Now()
	res, err := call()
	metrics.PaymentDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	outcome := string(res.Status)
	switch {
	case errors.Is(err, ErrUnknownOutcome):
		outcome = "unknown"
	case errors.Is(err, ErrNotFound):
		outcome = "not_found"
	case err != nil:
		outcome = "error"
	}
	metrics.PaymentCalls.WithLabelValues(op, outcome).Inc()
	return res, err
}

// NewProductionClient is the client every binary uses: HTTP to the provider,
// metrics per attempt, then retries and the circuit breaker around that.
func NewProductionClient(baseURL string, timeout time.Duration, transport func(*HTTPClient)) *Resilient {
	hc := NewHTTPClient(baseURL, timeout)
	if transport != nil {
		transport(hc)
	}
	b := NewBreaker(5, 10*time.Second)
	b.OnChange = func(s BreakerState) { metrics.CircuitState.Set(float64(s)) }
	return NewResilient(NewInstrumented(hc), DefaultRetry, b, func() { metrics.PaymentRetries.Inc() })
}
