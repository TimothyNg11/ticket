package payments

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// ErrCircuitOpen means the breaker is refusing calls because the provider has been
// failing. The request was never sent, so nothing was charged.
var ErrCircuitOpen = errors.New("payments: circuit open")

// RetryPolicy is exponential backoff with full jitter.
type RetryPolicy struct {
	Attempts int           // total tries, including the first
	Base     time.Duration // first backoff ceiling
	Max      time.Duration // largest backoff ceiling
}

// DefaultRetry tries three times, waiting up to 100ms, then 200ms.
var DefaultRetry = RetryPolicy{Attempts: 3, Base: 100 * time.Millisecond, Max: time.Second}

// backoff returns a random wait in [0, min(Max, Base*2^attempt)). Randomizing the
// whole interval ("full jitter") spreads retries from many clients apart, so a
// provider recovering from an outage isn't hit by synchronized waves.
func (p RetryPolicy) backoff(attempt int) time.Duration {
	ceiling := min(p.Max, p.Base<<attempt)
	return time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // jitter needs no cryptographic randomness
}

// BreakerState is the circuit breaker's state.
type BreakerState int

// Breaker states.
const (
	Closed   BreakerState = iota // calls flow; failures are counted
	Open                         // calls fail fast until the cooldown passes
	HalfOpen                     // one probe call is allowed through
)

func (s BreakerState) String() string { return [...]string{"closed", "open", "half_open"}[s] }

// Breaker opens after Threshold consecutive failures and stays open for Cooldown,
// then lets a single probe through: success closes it, failure reopens it.
// Without one, a dead provider makes every checkout wait for its full timeout and
// retries, tying up connections and threads across the whole API.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	OnChange  func(BreakerState) // optional, for metrics

	mu       sync.Mutex
	state    BreakerState
	failures int
	openedAt time.Time
	probing  bool
	now      func() time.Time
}

// NewBreaker returns a closed breaker.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{Threshold: threshold, Cooldown: cooldown, now: time.Now}
}

// State reports the current state.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick()
	return b.state
}

func (b *Breaker) tick() {
	if b.state == Open && b.now().Sub(b.openedAt) >= b.Cooldown {
		b.set(HalfOpen)
	}
}

func (b *Breaker) set(s BreakerState) {
	if b.state != s {
		b.state = s
		if b.OnChange != nil {
			b.OnChange(s)
		}
	}
}

// allow reports whether a call may proceed, reserving the probe slot in half-open.
func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick()
	switch b.state {
	case Closed:
		return true
	case HalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return false
	}
}

func (b *Breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if ok {
		b.failures = 0
		b.set(Closed)
		return
	}
	b.failures++
	if b.state == HalfOpen || b.failures >= b.Threshold {
		b.openedAt = b.now()
		b.set(Open)
	}
}

// Resilient wraps a Client with retries and a circuit breaker. Retrying a charge
// is only safe because every call carries an idempotency key: if the first try
// actually charged the card, the retry returns that same charge.
type Resilient struct {
	next    Client
	retry   RetryPolicy
	breaker *Breaker
	onRetry func()
}

// NewResilient wraps next.
func NewResilient(next Client, retry RetryPolicy, breaker *Breaker, onRetry func()) *Resilient {
	if onRetry == nil {
		onRetry = func() {}
	}
	return &Resilient{next: next, retry: retry, breaker: breaker, onRetry: onRetry}
}

// Ready returns ErrCircuitOpen while the breaker is open, so callers can refuse
// work before creating any state (checkout checks this before making an order).
func (r *Resilient) Ready() error {
	if r.breaker.State() == Open {
		return ErrCircuitOpen
	}
	return nil
}

// Charge implements Client.
func (r *Resilient) Charge(ctx context.Context, key string, amount int) (Result, error) {
	return r.do(ctx, func(ctx context.Context) (Result, error) { return r.next.Charge(ctx, key, amount) })
}

// Refund implements Client.
func (r *Resilient) Refund(ctx context.Context, key, chargeRef string, amount int) (Result, error) {
	return r.do(ctx, func(ctx context.Context) (Result, error) { return r.next.Refund(ctx, key, chargeRef, amount) })
}

// GetCharge implements Client.
func (r *Resilient) GetCharge(ctx context.Context, key string) (Result, error) {
	return r.do(ctx, func(ctx context.Context) (Result, error) { return r.next.GetCharge(ctx, key) })
}

func (r *Resilient) do(ctx context.Context, call func(context.Context) (Result, error)) (Result, error) {
	var lastErr error
	for attempt := range r.retry.Attempts {
		if attempt > 0 {
			r.onRetry()
			select {
			case <-ctx.Done():
				return Result{}, errors.Join(ErrUnknownOutcome, ctx.Err(), lastErr)
			case <-time.After(r.retry.backoff(attempt - 1)):
			}
		}
		if !r.breaker.allow() {
			if lastErr != nil {
				// An earlier try may have reached the provider: still unknown.
				return Result{}, lastErr
			}
			return Result{}, ErrCircuitOpen
		}
		res, err := call(ctx)
		// Only provider trouble counts against the breaker; a definite answer
		// (including "declined" or "not found") means the provider is healthy.
		r.breaker.record(!errors.Is(err, ErrUnknownOutcome))
		if !errors.Is(err, ErrUnknownOutcome) {
			return res, err
		}
		lastErr = err
	}
	return Result{}, lastErr
}
