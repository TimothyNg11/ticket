package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/db/sqlc"
	"ticket/internal/outbox"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func insertEvents(t *testing.T, q *sqlc.Queries, n int) []string {
	var ids []string
	for range n {
		id := uuid.NewString()
		b, _ := json.Marshal(map[string]any{"event_id": id, "order_id": uuid.NewString()})
		require.NoError(t, q.InsertOutbox(context.Background(), sqlc.InsertOutboxParams{
			AggregateID: uuid.New(), EventType: "order.confirmed", Payload: b}))
		ids = append(ids, id)
	}
	return ids
}

// The tests share one Redis, so each uses its own consumer group and counts only
// the events it inserted.
func collector(rdb *redis.Client, scope string, want map[string]bool) (outbox.Handler, func() map[string]int) {
	var mu sync.Mutex
	seen := map[string]int{}
	h := outbox.Once(rdb, scope, func(_ context.Context, e outbox.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if want[e.ID] {
			seen[e.ID]++
		}
		return nil
	})
	return h, func() map[string]int { mu.Lock(); defer mu.Unlock(); return seen }
}

func set(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func drain(t *testing.T, c *outbox.Consumer) {
	for {
		n, err := c.Poll(context.Background(), 100, 50*time.Millisecond)
		require.NoError(t, err)
		if n == 0 {
			return
		}
	}
}

func TestRelayPublishesOnceAndMarks(t *testing.T) {
	pool, rdb := pg.NewDB(t), testutil.NewRedis(t)
	ids := insertEvents(t, sqlc.New(pool), 25)
	r := outbox.NewRelay(pool, rdb)

	n, err := r.PublishBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	for {
		n, err = r.PublishBatch(context.Background(), 10)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	lag, err := r.Lag(context.Background())
	require.NoError(t, err)
	assert.Zero(t, lag, "nothing left unpublished")

	h, seen := collector(rdb, "t1", set(ids))
	c := outbox.NewConsumer(rdb, "group-"+uuid.NewString(), "c1", h, quiet)
	require.NoError(t, c.Ensure(context.Background()))
	drain(t, c)
	assert.Len(t, seen(), 25)
}

// TestCrashAfterPublishDuplicatesButHandledOnce simulates the relay dying between
// publishing and marking: the batch is published twice, yet each event is handled once.
func TestCrashAfterPublishDuplicatesButHandledOnce(t *testing.T) {
	pool, rdb := pg.NewDB(t), testutil.NewRedis(t)
	ids := insertEvents(t, sqlc.New(pool), 5)
	r := outbox.NewRelay(pool, rdb)
	_, err := r.PublishBatch(context.Background(), 100)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE outbox SET published_at = NULL`) // "the mark never happened"
	require.NoError(t, err)
	_, err = r.PublishBatch(context.Background(), 100)
	require.NoError(t, err)

	h, seen := collector(rdb, "t2-"+uuid.NewString(), set(ids))
	c := outbox.NewConsumer(rdb, "group-"+uuid.NewString(), "c1", h, quiet)
	require.NoError(t, c.Ensure(context.Background()))
	drain(t, c)
	for _, id := range ids {
		assert.Equal(t, 1, seen()[id], "event %s delivered twice, handled once", id)
	}
}

func TestFailedHandlerIsRetriedAndCrashedConsumerReclaimed(t *testing.T) {
	pool, rdb := pg.NewDB(t), testutil.NewRedis(t)
	ids := insertEvents(t, sqlc.New(pool), 1)
	_, err := outbox.NewRelay(pool, rdb).PublishBatch(context.Background(), 10)
	require.NoError(t, err)
	group := "group-" + uuid.NewString()

	// Consumer A takes the event and fails on it (as if it crashed mid-handling).
	failing := outbox.NewConsumer(rdb, group, "a", func(context.Context, outbox.Event) error { return errors.New("boom") }, quiet)
	require.NoError(t, failing.Ensure(context.Background()))
	n, err := failing.Poll(context.Background(), 100, 50*time.Millisecond)
	require.NoError(t, err)
	assert.Zero(t, n)

	// Consumer B reclaims it once it has been idle long enough.
	h, seen := collector(rdb, "t3-"+uuid.NewString(), set(ids))
	b := outbox.NewConsumer(rdb, group, "b", h, quiet)
	outbox.SetClaimIdle(b, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	drain(t, b)
	assert.Equal(t, 1, seen()[ids[0]])
}

func TestConcurrentRelaysDontDoublePublish(t *testing.T) {
	pool, rdb := pg.NewDB(t), testutil.NewRedis(t)
	insertEvents(t, sqlc.New(pool), 200)
	r := outbox.NewRelay(pool, rdb)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := r.PublishBatch(context.Background(), 20)
				assert.NoError(t, err)
				if n == 0 {
					return
				}
				mu.Lock()
				total += n
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 200, total)
}
