package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 4 done-when, part 1: repeated seat-map reads are served from cache.
func TestSeatMapReadsServedFromCache(t *testing.T) {
	e := newTestEnv(t)
	eventID, seats := saleEvent(t, e, 5)
	path := "/v1/events/" + eventID.String() + "/seatmap"
	for range 50 {
		status, _ := call(t, e.srv, "GET", path, "", nil)
		require.Equal(t, 200, status)
	}
	assert.EqualValues(t, 1, e.cache.Stats.Rebuilds.Load(), "one database read for 50 requests")

	// A hold shows up once the 250 ms rebuild-coalescing window has passed:
	// holding bumps the cache version (ADR 0009 bounds staleness at 2 s).
	tok := e.userToken(t)
	status, body := call(t, e.srv, "POST", "/v1/events/"+eventID.String()+"/holds", tok, map[string]any{"seat_ids": seats[:1]})
	require.Equal(t, 201, status, string(body))
	time.Sleep(300 * time.Millisecond)
	_, body = call(t, e.srv, "GET", path, "", nil)
	assert.Contains(t, string(body), `"state":"held"`)

	_, body = call(t, e.srv, "GET", "/v1/events/"+eventID.String(), "", nil)
	var ev struct {
		Available int `json:"available_seats"`
	}
	require.NoError(t, json.Unmarshal(body, &ev))
	assert.Equal(t, 4, ev.Available)
}

// Phase 4 done-when, part 2: bursts get 429 with Retry-After.
func TestBurstsReturn429(t *testing.T) {
	e := newTestEnvScaled(t, 1)
	eventID, seats := saleEvent(t, e, 10)
	tok := e.userToken(t)

	// Holds: 5 per minute per user. Each hold attempt targets a taken seat after
	// the first, so they fail fast with 409, but still spend tokens.
	statuses := map[int]int{}
	var retryAfter string
	for range 8 {
		status, body := call(t, e.srv, "POST", "/v1/events/"+eventID.String()+"/holds", tok, map[string]any{"seat_ids": seats[:1]})
		statuses[status]++
		if status == 429 {
			assert.Equal(t, "RATE_LIMITED", errCode(t, body))
		}
	}
	assert.Equal(t, 3, statuses[429], "requests 6-8 are limited")

	resp, err := e.srv.Client().Post(e.srv.URL+"/v1/auth/login", "application/json", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	for range 12 {
		resp, err = e.srv.Client().Post(e.srv.URL+"/v1/auth/login", "application/json", nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode == 429 {
			retryAfter = resp.Header.Get("Retry-After")
		}
	}
	require.NotEmpty(t, retryAfter, "login is limited per IP")
	secs, err := strconv.Atoi(retryAfter)
	require.NoError(t, err)
	assert.Positive(t, secs)
}

func TestLogoutRevokesAccessToken(t *testing.T) {
	e := newTestEnv(t)
	tp := registerAndLogin(t, e.srv, "alice@example.com")
	status, _ := call(t, e.srv, "GET", "/v1/orders", tp.AccessToken, nil)
	require.Equal(t, 200, status)

	status, _ = call(t, e.srv, "POST", "/v1/auth/logout", tp.AccessToken, map[string]string{"refresh_token": tp.RefreshToken})
	require.Equal(t, 204, status)
	status, body := call(t, e.srv, "GET", "/v1/orders", tp.AccessToken, nil)
	assert.Equal(t, 401, status, "the access token died with the session, not 15 minutes later")
	assert.Equal(t, "UNAUTHENTICATED", errCode(t, body))
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4321"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")

	assert.Equal(t, "10.0.0.5", (&Server{}).clientIP(r), "headers ignored unless behind a trusted proxy")
	behind := &Server{Deps: Deps{TrustProxy: true}}
	assert.Equal(t, "203.0.113.9", behind.clientIP(r), "right-most hop is what the ingress saw; the rest is spoofable")
}
