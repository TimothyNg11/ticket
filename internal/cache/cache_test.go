package cache_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/booking"
	"ticket/internal/cache"
	"ticket/internal/inventory"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	pool    *pgxpool.Pool
	c       *cache.Cache
	book    *booking.Service
	eventID uuid.UUID
	seats   []uuid.UUID
}

func newEnv(t *testing.T, rdb *redis.Client) *env {
	pool := pg.NewDB(t)
	inv := inventory.New(pool)
	ctx := context.Background()
	admin := testutil.CreateUser(t, pool, "admin@example.com", "admin")
	v, err := inv.CreateVenue(ctx, admin, inventory.VenueSpec{Name: "Hall", Timezone: "UTC",
		Sections: []inventory.SectionSpec{{Name: "Floor", Rows: []inventory.RowSpec{{Label: "A", SeatCount: 6}}}}})
	require.NoError(t, err)
	e, err := inv.CreateEvent(ctx, admin, inventory.EventSpec{VenueID: v.Venue.ID, Name: "Show",
		StartsAt: time.Now().Add(48 * time.Hour), OnSaleAt: time.Now().Add(-time.Hour),
		SectionPrices: map[uuid.UUID]int32{v.Sections[0].Section.ID: 100}})
	require.NoError(t, err)
	_, err = inv.PublishEvent(ctx, admin, e.ID)
	require.NoError(t, err)
	sm, err := inv.SeatMap(ctx, e.ID)
	require.NoError(t, err)
	var seats []uuid.UUID
	for _, s := range sm.Sections[0].Seats {
		seats = append(seats, s.EventSeatID)
	}
	c := cache.New(rdb, inv, quiet)
	book := booking.New(pool, nil, nil, 10*time.Minute)
	book.OnSeatsChanged(c.SeatsChanged)
	return &env{pool: pool, c: c, book: book, eventID: e.ID, seats: seats}
}

func TestSeatMapServedFromCache(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	ctx := context.Background()
	first, err := e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	for range 20 {
		sm, err := e.c.SeatMap(ctx, e.eventID)
		require.NoError(t, err)
		assert.Equal(t, first, sm)
	}
	assert.EqualValues(t, 1, e.c.Stats.Rebuilds.Load(), "only the first read touched Postgres")
	assert.EqualValues(t, 20, e.c.Stats.Hits.Load())
}

func TestSeatMapInvalidatedOnSeatChange(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	ctx := context.Background()
	_, err := e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)

	user := testutil.CreateUser(t, e.pool, "u@x.com", "user")
	_, err = e.book.CreateHold(ctx, user, e.eventID, e.seats[:1])
	require.NoError(t, err)

	sm, err := e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	assert.Equal(t, "held", sm.Sections[0].Seats[0].State, "the hold is visible immediately, not after the TTL")
}

func TestStampedeRebuildsOnce(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	var wg sync.WaitGroup
	// Stagger the readers across the rebuild window: requests that miss just as
	// the first rebuild finishes must find the filled key, not rebuild again.
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%10) * time.Millisecond)
			_, err := e.c.SeatMap(context.Background(), e.eventID)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, e.c.Stats.Rebuilds.Load(), "50 concurrent cold reads, one database query")
}

func TestOtherPodRebuildingServesStale(t *testing.T) {
	rdb := testutil.NewRedis(t)
	e := newEnv(t, rdb)
	ctx := context.Background()
	_, err := e.c.SeatMap(ctx, e.eventID) // fills the stale copy
	require.NoError(t, err)
	e.c.SeatsChanged(e.eventID, 0) // new version: current key is now empty
	// Pretend another pod holds the rebuild lock for version 1.
	require.NoError(t, rdb.Set(ctx, "lock:seatmap:"+e.eventID.String()+":v1", 1, time.Second).Err())

	_, err = e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, e.c.Stats.StaleServed.Load())
	assert.EqualValues(t, 1, e.c.Stats.Rebuilds.Load(), "no second database read")
}

func TestAvailabilityCounter(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	ctx := context.Background()
	n, err := e.c.Available(ctx, e.eventID)
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	user := testutil.CreateUser(t, e.pool, "u@x.com", "user")
	h, err := e.book.CreateHold(ctx, user, e.eventID, e.seats[:2])
	require.NoError(t, err)
	n, _ = e.c.Available(ctx, e.eventID)
	assert.Equal(t, 4, n)

	require.NoError(t, e.book.ReleaseHold(ctx, user, h.ID))
	n, _ = e.c.Available(ctx, e.eventID)
	assert.Equal(t, 6, n)
}

func TestRecomputeFixesDrift(t *testing.T) {
	rdb := testutil.NewRedis(t)
	e := newEnv(t, rdb)
	ctx := context.Background()
	require.NoError(t, rdb.Set(ctx, "avail:"+e.eventID.String(), 999, 0).Err())
	_, err := e.c.RecomputeAvailability(ctx)
	require.NoError(t, err)
	n, _ := e.c.Available(ctx, e.eventID)
	assert.Equal(t, 6, n)
}

func TestRevocation(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	ctx := context.Background()
	jti := uuid.NewString()
	assert.False(t, e.c.IsRevoked(ctx, jti))
	require.NoError(t, e.c.Revoke(ctx, jti, time.Now().Add(time.Minute)))
	assert.True(t, e.c.IsRevoked(ctx, jti))
}

func TestRedisDownFallsBackToPostgres(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	e := newEnv(t, dead)
	ctx := context.Background()
	sm, err := e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	assert.Len(t, sm.Sections[0].Seats, 6)
	_, err = e.c.Event(ctx, e.eventID)
	require.NoError(t, err)
	n, err := e.c.Available(ctx, e.eventID)
	require.NoError(t, err)
	assert.Equal(t, 6, n)
	assert.False(t, e.c.IsRevoked(ctx, "x"))
	assert.Positive(t, e.c.Stats.Errors.Load())
}
