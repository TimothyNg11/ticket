package waitingroom_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/auth"
	"ticket/internal/testutil"
	"ticket/internal/waitingroom"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

var secret = []byte("0123456789abcdef0123456789abcdef")

func newRoom(t *testing.T) *waitingroom.Room {
	return waitingroom.New(testutil.NewRedis(t), auth.NewPassIssuer(secret))
}

var rate = waitingroom.Rate{Batch: 2, Interval: 10 * time.Second}

func TestJoinIsFirstComeFirstServedAndIdempotent(t *testing.T) {
	r := newRoom(t)
	ctx := context.Background()
	ev := uuid.New()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{a, b, c} {
		p, err := r.Join(ctx, ev, u, rate)
		require.NoError(t, err)
		assert.Equal(t, i+1, p.Position)
		time.Sleep(2 * time.Millisecond) // distinct arrival milliseconds
	}
	again, err := r.Join(ctx, ev, a, rate)
	require.NoError(t, err)
	assert.Equal(t, 1, again.Position, "rejoining keeps your place")

	p, err := r.Join(ctx, ev, uuid.New(), rate)
	require.NoError(t, err)
	assert.Equal(t, 4, p.Position)
	assert.Equal(t, 20*time.Second, p.EstimatedWait, "position 4 at 2 per 10s")
}

func TestAdmitInBatchesOncePerInterval(t *testing.T) {
	r := newRoom(t)
	ctx := context.Background()
	ev := uuid.New()
	users := make([]uuid.UUID, 5)
	tokens := make([]string, 5)
	for i := range users {
		users[i] = uuid.New()
		p, err := r.Join(ctx, ev, users[i], rate)
		require.NoError(t, err)
		tokens[i] = p.QueueToken
		time.Sleep(2 * time.Millisecond)
	}
	n, err := r.AdmitNext(ctx, ev, rate)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = r.AdmitNext(ctx, ev, rate)
	require.NoError(t, err)
	assert.Zero(t, n, "the next batch waits for the interval")

	rateOf := func(uuid.UUID) waitingroom.Rate { return rate }
	_, first, err := r.Status(ctx, tokens[0], rateOf)
	require.NoError(t, err)
	assert.True(t, first.Admitted, "earliest arrivals are admitted first")
	require.NoError(t, r.CheckAdmission(first.AdmissionToken, users[0], ev))

	_, third, err := r.Status(ctx, tokens[2], rateOf)
	require.NoError(t, err)
	assert.False(t, third.Admitted)
	assert.Equal(t, 1, third.Position, "moved to the front")

	length, _ := r.Length(ctx, ev)
	assert.EqualValues(t, 3, length)
}

func TestAdmissionIsBoundToUserAndEvent(t *testing.T) {
	r := newRoom(t)
	ctx := context.Background()
	ev, u := uuid.New(), uuid.New()
	_, err := r.Join(ctx, ev, u, rate)
	require.NoError(t, err)
	_, err = r.AdmitNext(ctx, ev, rate)
	require.NoError(t, err)
	p, err := r.Join(ctx, ev, u, rate)
	require.NoError(t, err)
	require.True(t, p.Admitted)

	assert.NoError(t, r.CheckAdmission(p.AdmissionToken, u, ev))
	assert.Error(t, r.CheckAdmission(p.AdmissionToken, uuid.New(), ev), "can't be shared with another user")
	assert.Error(t, r.CheckAdmission(p.AdmissionToken, u, uuid.New()), "can't be used for another event")
	assert.Error(t, r.CheckAdmission(p.QueueToken, u, ev), "a queue token is not an admission pass")
}

// TestConcurrentAdmittersAdmitOneBatch: several admitter replicas ticking at the
// same moment must admit exactly one batch between them.
func TestConcurrentAdmittersAdmitOneBatch(t *testing.T) {
	r := newRoom(t)
	ctx := context.Background()
	ev := uuid.New()
	for range 20 {
		_, err := r.Join(ctx, ev, uuid.New(), rate)
		require.NoError(t, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := r.AdmitNext(ctx, ev, waitingroom.Rate{Batch: 4, Interval: 10 * time.Second})
			assert.NoError(t, err)
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, 4, total)
}

// The waiting room tells each buyer how long to wait before polling again:
// people far back in line don't need updates every few seconds, and 50,000
// buyers polling at a fixed fast rate overloaded the system (Phase 9).
func TestPollAfterScalesWithPlaceInLine(t *testing.T) {
	r := waitingroom.Rate{Batch: 500, Interval: 10 * time.Second}
	assert.Equal(t, 5*time.Second, r.PollAfter(1), "near the front: poll often")
	assert.Equal(t, 20*time.Second, r.PollAfter(2000), "4 batches away (40s): poll every 20s")
	assert.Equal(t, 120*time.Second, r.PollAfter(40000), "800s away: poll every 2 minutes")
	assert.Equal(t, 5*time.Second, waitingroom.Rate{}.PollAfter(10), "unknown rate: default")
}
