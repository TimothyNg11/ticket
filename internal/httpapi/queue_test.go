package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/auth"
	"ticket/internal/inventory"
	"ticket/internal/testutil"
	"ticket/internal/waitingroom"
)

// queuedEvent creates an on-sale event with a waiting room and n seats.
func queuedEvent(t *testing.T, e *testEnv, n int, batch int32) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	inv := inventory.New(e.pool)
	admin := testutil.CreateUser(t, e.pool, "queue-admin@example.com", "admin")
	v, err := inv.CreateVenue(ctx, admin, inventory.VenueSpec{Name: "Stadium", Timezone: "UTC",
		Sections: []inventory.SectionSpec{{Name: "Floor", Rows: []inventory.RowSpec{{Label: "A", SeatCount: n}}}}})
	require.NoError(t, err)
	ev, err := inv.CreateEvent(ctx, admin, inventory.EventSpec{VenueID: v.Venue.ID, Name: "Tour",
		StartsAt: time.Now().Add(48 * time.Hour), OnSaleAt: time.Now().Add(-time.Minute),
		SectionPrices: map[uuid.UUID]int32{v.Sections[0].Section.ID: 9900},
		Queue:         inventory.QueueSettings{Enabled: true, Batch: batch, IntervalSeconds: 10}})
	require.NoError(t, err)
	_, err = inv.PublishEvent(ctx, admin, ev.ID)
	require.NoError(t, err)
	sm, err := inv.SeatMap(ctx, ev.ID)
	require.NoError(t, err)
	var ids []uuid.UUID
	for _, s := range sm.Sections[0].Seats {
		ids = append(ids, s.EventSeatID)
	}
	return ev.ID, ids
}

// bulkUsers inserts n users in one statement and returns access tokens for them.
func bulkUsers(t *testing.T, e *testEnv, n int) ([]uuid.UUID, []string) {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		INSERT INTO users (email, password_hash)
		SELECT 'bulk-' || g || '-' || gen_random_uuid() || '@example.com', 'x' FROM generate_series(1, $1) g
		RETURNING id`, n)
	require.NoError(t, err)
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	toks := make([]string, len(ids))
	for i, id := range ids {
		toks[i], err = e.tokens.Issue(id, "user")
		require.NoError(t, err)
	}
	return ids, toks
}

func TestQueueRequiredForHolds(t *testing.T) {
	e := newTestEnv(t)
	eventID, seats := queuedEvent(t, e, 5, 1)
	ids, toks := bulkUsers(t, e, 2)
	holdPath := "/v1/events/" + eventID.String() + "/holds"

	status, body := call(t, e.srv, "POST", holdPath, toks[0], map[string]any{"seat_ids": seats[:1]})
	assert.Equal(t, 403, status)
	assert.Equal(t, "ADMISSION_REQUIRED", errCode(t, body))

	// Both join; one batch of 1 admits the first.
	var joined [2]struct {
		QueueToken string `json:"queue_token"`
		Position   int    `json:"position"`
	}
	for i := range 2 {
		status, body = call(t, e.srv, "POST", "/v1/events/"+eventID.String()+"/queue", toks[i], nil)
		require.Equal(t, 200, status, string(body))
		require.NoError(t, json.Unmarshal(body, &joined[i]))
		assert.Equal(t, i+1, joined[i].Position)
		time.Sleep(2 * time.Millisecond)
	}
	room := waitingroom.New(testutil.NewRedis(t), auth.NewPassIssuer(testSecret))
	n, err := room.AdmitNext(context.Background(), eventID, waitingroom.Rate{Batch: 1, Interval: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	status, body = call(t, e.srv, "GET", "/v1/queue/"+joined[0].QueueToken, toks[0], nil)
	require.Equal(t, 200, status, string(body))
	var st struct {
		Admitted       bool   `json:"admitted"`
		AdmissionToken string `json:"admission_token"`
	}
	require.NoError(t, json.Unmarshal(body, &st))
	require.True(t, st.Admitted)

	status, _ = call(t, e.srv, "GET", "/v1/queue/"+joined[0].QueueToken, toks[1], nil)
	assert.Equal(t, 404, status, "someone else's queue token reveals nothing")

	hold := func(tok, admission string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+holdPath, jsonBody(t, map[string]any{"seat_ids": seats[:1]}))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set("Content-Type", "application/json")
		if admission != "" {
			req.Header.Set("Admission-Token", admission)
		}
		resp, err := e.srv.Client().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, 403, hold(toks[1], st.AdmissionToken), "an admission token can't be borrowed")
	assert.Equal(t, 201, hold(toks[0], st.AdmissionToken))
	_ = ids
}

// TestQueueBurst10000 is Phase 5's done-when: 10,000 users rush the waiting room,
// one batch of 500 is admitted, then everyone tries to hold a seat. Only the
// admitted 500 get past the gate.
func TestQueueBurst10000(t *testing.T) {
	const users, batch = 10_000, 500
	e := newTestEnv(t)
	eventID, seats := queuedEvent(t, e, users, batch)
	ids, toks := bulkUsers(t, e, users)
	e.srv.Client().Transport.(*http.Transport).MaxConnsPerHost = 64

	// 1. Everyone joins at once.
	parallel(users, 64, func(i int) {
		status, _ := call(t, e.srv, "POST", "/v1/events/"+eventID.String()+"/queue", toks[i], nil)
		assert.Equal(t, 200, status)
	})

	// 2. The admitter runs one tick.
	room := waitingroom.New(testutil.NewRedis(t), auth.NewPassIssuer(testSecret))
	n, err := room.AdmitNext(context.Background(), eventID, waitingroom.Rate{Batch: batch, Interval: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, batch, n)
	waiting, err := room.Length(context.Background(), eventID)
	require.NoError(t, err)
	assert.EqualValues(t, users-batch, waiting)

	// 3. Everyone polls for admission and tries to hold "their" seat.
	var created, forbidden, other atomic.Int64
	admittedUsers := sync.Map{}
	parallel(users, 64, func(i int) {
		p, err := room.Join(context.Background(), eventID, ids[i], waitingroom.Rate{Batch: batch, Interval: 10 * time.Second})
		if !assert.NoError(t, err) {
			return
		}
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/events/"+eventID.String()+"/holds",
			jsonBody(t, map[string]any{"seat_ids": []uuid.UUID{seats[i]}}))
		req.Header.Set("Authorization", "Bearer "+toks[i])
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set("Content-Type", "application/json")
		if p.Admitted {
			admittedUsers.Store(ids[i], true)
			req.Header.Set("Admission-Token", p.AdmissionToken)
		}
		resp, err := e.srv.Client().Do(req)
		if !assert.NoError(t, err) {
			return
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case 201:
			created.Add(1)
		case 403:
			forbidden.Add(1)
		default:
			other.Add(1)
		}
	})
	assert.EqualValues(t, batch, created.Load(), "every admitted user got their seat")
	assert.EqualValues(t, users-batch, forbidden.Load(), "everyone else was turned away at the gate")
	assert.Zero(t, other.Load())

	// The database agrees: exactly 500 holds, all by admitted users.
	rows, err := e.pool.Query(context.Background(), `SELECT user_id FROM holds WHERE event_id = $1`, eventID)
	require.NoError(t, err)
	holders := 0
	for rows.Next() {
		var u uuid.UUID
		require.NoError(t, rows.Scan(&u))
		_, ok := admittedUsers.Load(u)
		assert.True(t, ok, "hold by a user who was never admitted")
		holders++
	}
	assert.Equal(t, batch, holders)
}

// parallel runs fn(0..n-1) on `workers` goroutines.
func parallel(n, workers int, fn func(i int)) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}
