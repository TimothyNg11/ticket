package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/inventory"
	"ticket/internal/testutil"
)

// saleEvent creates a published, on-sale event with n seats directly through the
// services (faster than HTTP for big fixtures) and returns its id and seat ids.
func saleEvent(t *testing.T, e *testEnv, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	inv := inventory.New(e.pool)
	admin := testutil.CreateUser(t, e.pool, "seed-admin@example.com", "admin")
	v, err := inv.CreateVenue(ctx, admin, inventory.VenueSpec{Name: "Arena", Timezone: "UTC",
		Sections: []inventory.SectionSpec{{Name: "Floor", Rows: []inventory.RowSpec{{Label: "A", SeatCount: n}}}}})
	require.NoError(t, err)
	ev, err := inv.CreateEvent(ctx, admin, inventory.EventSpec{VenueID: v.Venue.ID, Name: "Show",
		StartsAt: time.Now().Add(48 * time.Hour), OnSaleAt: time.Now().Add(-time.Hour),
		SectionPrices: map[uuid.UUID]int32{v.Sections[0].Section.ID: 5000}})
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

// userToken creates a user directly and returns a valid access token for them.
func (e *testEnv) userToken(t *testing.T) string {
	t.Helper()
	id := testutil.CreateUser(t, e.pool, uuid.NewString()[:12]+"@example.com", "user")
	tok, err := e.tokens.Issue(id, "user")
	require.NoError(t, err)
	return tok
}

func TestHoldCheckoutCancelOverHTTP(t *testing.T) {
	e := newTestEnv(t)
	eventID, seats := saleEvent(t, e, 4)
	tok := e.userToken(t)

	status, body := call(t, e.srv, "POST", "/v1/events/"+eventID.String()+"/holds", tok, map[string]any{"seat_ids": seats[:2]})
	require.Equal(t, 201, status, string(body))
	var hold struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &hold))

	status, body = callWithKey(t, e.srv, "POST", "/v1/holds/"+hold.ID+"/checkout", tok, "checkout-1", nil)
	require.Equal(t, 201, status, string(body))
	var order struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Tickets []struct {
			QR string `json:"qr_token"`
		} `json:"tickets"`
	}
	require.NoError(t, json.Unmarshal(body, &order))
	assert.Equal(t, "confirmed", order.Status)
	assert.Len(t, order.Tickets, 2)

	// A retried checkout with the same key replays the response; no second charge.
	status2, body2 := callWithKey(t, e.srv, "POST", "/v1/holds/"+hold.ID+"/checkout", tok, "checkout-1", nil)
	assert.Equal(t, 201, status2)
	assert.JSONEq(t, string(body), string(body2))
	assert.Equal(t, 1, e.pay.Charges())

	status, body = call(t, e.srv, "GET", "/v1/orders/"+order.ID, e.userToken(t), nil)
	assert.Equal(t, 404, status, "another user can't see it")
	assert.Equal(t, "NOT_FOUND", errCode(t, body))
	status, body = call(t, e.srv, "GET", "/v1/orders", tok, nil)
	require.Equal(t, 200, status)
	assert.Contains(t, string(body), order.ID)

	status, body = call(t, e.srv, "POST", "/v1/orders/"+order.ID+"/cancel", tok, nil)
	require.Equal(t, 200, status, string(body))
	assert.Contains(t, string(body), `"status":"refunded"`)
}

func TestBookingRequiresAuthAndIdempotencyKey(t *testing.T) {
	e := newTestEnv(t)
	eventID, seats := saleEvent(t, e, 2)
	path := "/v1/events/" + eventID.String() + "/holds"
	body := map[string]any{"seat_ids": seats[:1]}

	status, resp := call(t, e.srv, "POST", path, "", body)
	assert.Equal(t, 401, status)
	assert.Equal(t, "UNAUTHENTICATED", errCode(t, resp))

	status, resp = callWithKey(t, e.srv, "POST", path, e.userToken(t), "", body)
	assert.Equal(t, 400, status)
	assert.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", errCode(t, resp))
}

func TestHoldValidation(t *testing.T) {
	e := newTestEnv(t)
	eventID, seats := saleEvent(t, e, 10)
	tok := e.userToken(t)
	path := "/v1/events/" + eventID.String() + "/holds"
	for name, ids := range map[string][]uuid.UUID{
		"none":      {},
		"nine":      seats[:9],
		"duplicate": {seats[0], seats[0]},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := call(t, e.srv, "POST", path, tok, map[string]any{"seat_ids": ids})
			assert.Equal(t, 400, status)
			assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
		})
	}
}

// TestFlashSaleHolds500Parallel is Phase 2's "done when": 500 users request one
// of the same 10 seats at the same moment. Exactly 10 may win.
func TestFlashSaleHolds500Parallel(t *testing.T) {
	e := newTestEnv(t)
	eventID, seats := saleEvent(t, e, 10)
	tokens := make([]string, 500)
	for i := range tokens {
		tokens[i] = e.userToken(t)
	}

	// 500 goroutines fire at once. The client shares at most 100 sockets (500
	// simultaneous dials overflow the OS listen backlog on Windows), so requests
	// queue in the client but all race for the same rows in Postgres.
	e.srv.Client().Transport.(*http.Transport).MaxConnsPerHost = 100

	path := "/v1/events/" + eventID.String() + "/holds"
	statuses := make([]int, len(tokens))
	codes := make([]string, len(tokens))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, tok := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every request at once
			status, body := call(t, e.srv, "POST", path, tok, map[string]any{"seat_ids": []uuid.UUID{seats[i%10]}})
			statuses[i] = status
			if status != 201 {
				var b struct {
					Error struct{ Code string } `json:"error"`
				}
				_ = json.Unmarshal(body, &b)
				codes[i] = b.Error.Code
			}
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for i, s := range statuses {
		switch s {
		case 201:
			won++
		case 409:
			assert.Equal(t, "SEAT_UNAVAILABLE", codes[i])
		default:
			t.Errorf("request %d: unexpected status %d (%s)", i, s, codes[i])
		}
	}
	assert.Equal(t, 10, won, "exactly one winner per seat")

	var held, holds int
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT count(*), count(DISTINCT hold_id) FROM event_seats WHERE event_id = $1 AND state = 'held'`, eventID).Scan(&held, &holds))
	assert.Equal(t, 10, held)
	assert.Equal(t, 10, holds, "each seat held by a different hold")
	fmt.Printf("flash sale: 500 requests, %d winners\n", won)
}
