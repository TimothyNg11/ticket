package payments_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/payments"
	"ticket/internal/payments/mock"
)

// fake is a scripted Client: each call takes the next error from errs (nil =
// success), and counts calls.
type fake struct {
	errs  []error
	calls atomic.Int64
}

func (f *fake) next() (payments.Result, error) {
	n := int(f.calls.Add(1)) - 1
	if n < len(f.errs) && f.errs[n] != nil {
		return payments.Result{}, f.errs[n]
	}
	return payments.Result{Status: payments.Succeeded, Ref: "ch_1"}, nil
}
func (f *fake) Charge(context.Context, string, int) (payments.Result, error) { return f.next() }
func (f *fake) Refund(context.Context, string, string, int) (payments.Result, error) {
	return f.next()
}
func (f *fake) GetCharge(context.Context, string) (payments.Result, error) { return f.next() }

var fastRetry = payments.RetryPolicy{Attempts: 3, Base: time.Millisecond, Max: 5 * time.Millisecond}

func TestRetriesUnknownOutcomes(t *testing.T) {
	f := &fake{errs: []error{payments.ErrUnknownOutcome, payments.ErrUnknownOutcome}}
	r := payments.NewResilient(f, fastRetry, payments.NewBreaker(10, time.Second), nil)
	res, err := r.Charge(context.Background(), "k", 100)
	require.NoError(t, err)
	assert.Equal(t, payments.Succeeded, res.Status)
	assert.EqualValues(t, 3, f.calls.Load())
}

func TestDoesNotRetryDefiniteAnswers(t *testing.T) {
	f := &fake{errs: []error{payments.ErrNotFound}}
	r := payments.NewResilient(f, fastRetry, payments.NewBreaker(10, time.Second), nil)
	_, err := r.GetCharge(context.Background(), "k")
	assert.ErrorIs(t, err, payments.ErrNotFound)
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestGivesUpAsUnknown(t *testing.T) {
	f := &fake{errs: []error{payments.ErrUnknownOutcome, payments.ErrUnknownOutcome, payments.ErrUnknownOutcome}}
	r := payments.NewResilient(f, fastRetry, payments.NewBreaker(10, time.Second), nil)
	_, err := r.Charge(context.Background(), "k", 100)
	assert.ErrorIs(t, err, payments.ErrUnknownOutcome, "after retries the outcome is still unknown, never 'declined'")
}

func TestBreakerOpensFailsFastAndRecovers(t *testing.T) {
	down := make([]error, 100)
	for i := range down {
		down[i] = payments.ErrUnknownOutcome
	}
	f := &fake{errs: down}
	b := payments.NewBreaker(5, 50*time.Millisecond)
	var states []payments.BreakerState
	b.OnChange = func(s payments.BreakerState) { states = append(states, s) }
	r := payments.NewResilient(f, payments.RetryPolicy{Attempts: 1}, b, nil)
	ctx := context.Background()

	for range 5 {
		_, err := r.Charge(ctx, "k", 100)
		require.ErrorIs(t, err, payments.ErrUnknownOutcome)
	}
	assert.Equal(t, payments.Open, b.State())
	assert.ErrorIs(t, r.Ready(), payments.ErrCircuitOpen)
	calls := f.calls.Load()
	_, err := r.Charge(ctx, "k", 100)
	assert.ErrorIs(t, err, payments.ErrCircuitOpen)
	assert.Equal(t, calls, f.calls.Load(), "open circuit: the provider isn't called at all")

	// Provider recovers; after the cooldown one probe goes through and closes it.
	f.errs = nil
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, payments.HalfOpen, b.State())
	_, err = r.Charge(ctx, "k", 100)
	require.NoError(t, err)
	assert.Equal(t, payments.Closed, b.State())
	assert.Equal(t, []payments.BreakerState{payments.Open, payments.HalfOpen, payments.Closed}, states)
}

func TestFailedProbeReopens(t *testing.T) {
	f := &fake{errs: []error{payments.ErrUnknownOutcome, payments.ErrUnknownOutcome}}
	b := payments.NewBreaker(1, 20*time.Millisecond)
	r := payments.NewResilient(f, payments.RetryPolicy{Attempts: 1}, b, nil)
	_, _ = r.Charge(context.Background(), "k", 1)
	time.Sleep(25 * time.Millisecond)
	_, err := r.Charge(context.Background(), "k", 1)
	assert.ErrorIs(t, err, payments.ErrUnknownOutcome)
	assert.Equal(t, payments.Open, b.State())
}

// TestRetryAgainstFlakyProviderChargesOnce: the provider records the charge and
// then fails the response; the retry must get the same charge back, not a new one.
func TestRetryAgainstFlakyProviderChargesOnce(t *testing.T) {
	// error roll yes, record-first yes, decline no; then everything clean
	m, c := setup(t, mock.Config{ErrorRate: 0.5}, seq(0.1, 0.1, 0.9, 0.9, 0.9, 0.9))
	r := payments.NewResilient(c, fastRetry, payments.NewBreaker(10, time.Second), nil)
	res, err := r.Charge(context.Background(), "order-1", 500)
	require.NoError(t, err)
	assert.Equal(t, payments.Succeeded, res.Status)
	assert.Equal(t, 1, m.Charges())
}
