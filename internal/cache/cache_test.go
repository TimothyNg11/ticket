package cache_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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

	// Within the 250 ms coalescing window a reader may still get the copy built
	// just before the hold; after it, the hold must be visible. Never the 2 s TTL.
	time.Sleep(300 * time.Millisecond)
	sm, err := e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	assert.Equal(t, "held", sm.Sections[0].Seats[0].State, "visible once the coalescing window has passed")
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

func TestOtherPodRebuildingServesLatest(t *testing.T) {
	rdb := testutil.NewRedis(t)
	e := newEnv(t, rdb)
	ctx := context.Background()
	_, err := e.c.SeatMap(ctx, e.eventID) // builds v0 and the "latest" copy
	require.NoError(t, err)
	// A change makes v1 current, and the rebuild window has passed...
	e.c.SeatsChanged(e.eventID, 0)
	require.NoError(t, rdb.Del(ctx, "seatmap:"+e.eventID.String()+":fresh").Err())
	// ...but another pod holds the rebuild lock for v1.
	require.NoError(t, rdb.Set(ctx, "lock:seatmap:"+e.eventID.String()+":v1", 1, time.Second).Err())

	_, err = e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, e.c.Stats.StaleServed.Load())
	assert.EqualValues(t, 1, e.c.Stats.Rebuilds.Load(), "no second database read")
}

// Under a burst of purchases the version changes many times a second. Rebuilds
// are coalesced: within 250 ms of a rebuild, readers get that copy instead of
// rebuilding again, which bounds database load per event.
func TestRebuildsAreCoalescedUnderWrites(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	ctx := context.Background()
	for range 20 {
		e.c.SeatsChanged(e.eventID, 0)
		_, err := e.c.SeatMap(ctx, e.eventID)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, e.c.Stats.Rebuilds.Load(), "20 changes in quick succession, one rebuild")
	assert.EqualValues(t, 19, e.c.Stats.Coalesced.Load())

	time.Sleep(300 * time.Millisecond) // past the coalescing window
	e.c.SeatsChanged(e.eventID, 0)
	_, err := e.c.SeatMap(ctx, e.eventID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, e.c.Stats.Rebuilds.Load(), "the next change after the window is rebuilt")
}

// The cache hands out the finished, gzip-compressed response body, so serving
// a seat map costs no JSON work per request.
func TestSeatMapJSONIsGzippedAPIShape(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	gz, err := e.c.SeatMapJSON(context.Background(), e.eventID)
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	var body struct {
		EventID  string `json:"event_id"`
		Sections []struct {
			Seats []struct {
				EventSeatID string `json:"event_seat_id"`
				PriceCents  int    `json:"price_cents"`
				State       string `json:"state"`
			} `json:"seats"`
		} `json:"sections"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, e.eventID.String(), body.EventID)
	assert.Len(t, body.Sections[0].Seats, 6)
	assert.Equal(t, 100, body.Sections[0].Seats[0].PriceCents)
	assert.Less(t, len(gz), len(raw), "compressed")
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

// Requests that wait on a shared rebuild must not fail because the request
// that started it went away. Phase 9 load tests found the opposite: the
// rebuild ran on the first caller's context, so one disconnect failed every
// waiter with "context canceled" (thousands of 500s under load).
func TestSharedRebuildSurvivesLeaderDisconnect(t *testing.T) {
	e := newEnv(t, testutil.NewRedis(t))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cache.SetBuildHook(e.c, func() {
		once.Do(func() { close(started) })
		<-release
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = e.c.SeatMapJSON(leaderCtx, e.eventID)
	}()
	<-started // the leader is inside the rebuild

	followerErr := make(chan error, 1)
	go func() {
		_, err := e.c.SeatMapJSON(context.Background(), e.eventID)
		followerErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the follower join the flight
	cancelLeader()                    // the first client hangs up
	close(release)

	require.NoError(t, <-followerErr, "a waiter must not inherit the leader's cancellation")
	<-leaderDone
}
