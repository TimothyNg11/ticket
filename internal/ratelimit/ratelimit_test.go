package ratelimit_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/ratelimit"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

func TestBurstThenDeny(t *testing.T) {
	l := ratelimit.New(testutil.NewRedis(t), 1, nil)
	r := ratelimit.PerMinute("holds", 5)
	key := uuid.NewString()
	for i := range 5 {
		assert.True(t, l.Allow(context.Background(), r, key).Allowed, "request %d", i)
	}
	d := l.Allow(context.Background(), r, key)
	assert.False(t, d.Allowed)
	assert.InDelta(t, 12*time.Second, d.RetryAfter, float64(time.Second), "one token per 12s at 5/min")

	assert.True(t, l.Allow(context.Background(), r, uuid.NewString()).Allowed, "other keys have their own bucket")
}

func TestRefills(t *testing.T) {
	l := ratelimit.New(testutil.NewRedis(t), 1, nil)
	r := ratelimit.Rule{Name: "fast", Rate: 20, Burst: 1}
	key := uuid.NewString()
	require.True(t, l.Allow(context.Background(), r, key).Allowed)
	require.False(t, l.Allow(context.Background(), r, key).Allowed)
	time.Sleep(80 * time.Millisecond)
	assert.True(t, l.Allow(context.Background(), r, key).Allowed)
}

// TestAtomicUnderConcurrency: 100 simultaneous requests against a burst of 10 must
// allow exactly 10. A non-atomic read-modify-write would let extra ones through.
func TestAtomicUnderConcurrency(t *testing.T) {
	l := ratelimit.New(testutil.NewRedis(t), 1, nil)
	r := ratelimit.Rule{Name: "race", Rate: 0.001, Burst: 10}
	key := uuid.NewString()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow(context.Background(), r, key).Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 10, allowed.Load())
}

func TestScaleMultipliesRules(t *testing.T) {
	l := ratelimit.New(testutil.NewRedis(t), 3, nil)
	r := ratelimit.Rule{Name: "scaled", Rate: 0.001, Burst: 2}
	key := uuid.NewString()
	n := 0
	for range 10 {
		if l.Allow(context.Background(), r, key).Allowed {
			n++
		}
	}
	assert.Equal(t, 6, n)
}

func TestFallsBackWhenRedisDown(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	var errs atomic.Int64
	l := ratelimit.New(dead, 1, func(error) { errs.Add(1) })
	r := ratelimit.Rule{Name: "down", Rate: 0.001, Burst: 8}
	n := 0
	for range 10 {
		if l.Allow(context.Background(), r, "k").Allowed {
			n++
		}
	}
	assert.Equal(t, 2, n, "fallback allows a quarter of the burst")
	assert.EqualValues(t, 10, errs.Load())
}
