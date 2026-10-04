package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/apperr"
)

// Postgres rejects NUL bytes in text; that must surface as a client error, not a 500.
func TestNULByteIs400NotInternal(t *testing.T) {
	srv := newTestServer(t)
	status, body := call(t, srv, "POST", "/v1/auth/login", "", map[string]string{"email": "a\u0000@x.com", "password": pw})
	assert.Equal(t, 400, status, string(body))
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))

	admin := registerAndLogin(t, srv, testAdminEmail)
	v := venueBody()
	v["name"] = "Hall\u0000"
	status, body = call(t, srv, "POST", "/v1/admin/venues", admin.AccessToken, v)
	assert.Equal(t, 400, status, string(body))
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}

func TestOversizedBodyIs413(t *testing.T) {
	srv := newTestServer(t)
	big := `{"email":"a@x.com","password":"` + strings.Repeat("x", 2<<20) + `"}`
	resp, err := srv.Client().Post(srv.URL+"/v1/auth/register", "application/json", bytes.NewBufferString(big))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 413, resp.StatusCode)
	assert.Equal(t, "REQUEST_TOO_LARGE", errCode(t, body))
}

func TestUnknownRouteAndMethodUseErrorShape(t *testing.T) {
	srv := newTestServer(t)
	status, body := call(t, srv, "GET", "/v1/nope", "", nil)
	assert.Equal(t, 404, status)
	assert.Equal(t, "NOT_FOUND", errCode(t, body))

	status, body = call(t, srv, "DELETE", "/v1/events", "", nil)
	assert.Equal(t, 405, status)
	assert.Equal(t, "METHOD_NOT_ALLOWED", errCode(t, body))
}

func TestPanicIsJSON500(t *testing.T) {
	s := &Server{Deps: Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	h := middleware.RequestID(s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, 500, rec.Code)
	assert.Equal(t, "INTERNAL", errCode(t, rec.Body.Bytes()))
}

// During shutdown the pod fails readiness (so it leaves the load balancer) but
// stays live and keeps serving the requests still reaching it.
func TestDrainingFailsReadinessOnly(t *testing.T) {
	e := newTestEnv(t)
	e.draining.Store(true)
	status, body := call(t, e.srv, "GET", "/readyz", "", nil)
	assert.Equal(t, 503, status)
	assert.Equal(t, "DEPENDENCY_DOWN", errCode(t, body))
	status, _ = call(t, e.srv, "GET", "/healthz", "", nil)
	assert.Equal(t, 200, status, "liveness stays green: don't restart a pod that is shutting down cleanly")
	status, _ = call(t, e.srv, "GET", "/v1/events", "", nil)
	assert.Equal(t, 200, status, "in-flight traffic is still served")
}

func TestMetricsUseRoutePatternsAndRequestIDHeader(t *testing.T) {
	e := newTestEnv(t)
	resp, err := e.srv.Client().Get(e.srv.URL + "/v1/events/00000000-0000-0000-0000-000000000009")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"), "clients can quote the request id")

	status, body := call(t, e.srv, "GET", "/metrics", "", nil)
	require.Equal(t, 200, status)
	assert.Contains(t, string(body), `ticket_http_requests_total{method="GET",route="/v1/events/{id}",status="404"}`,
		"labelled by route pattern, not by the raw id")
	assert.NotContains(t, string(body), "00000000-0000-0000-0000-000000000009")
	assert.Contains(t, string(body), "ticket_http_request_duration_seconds_bucket")
}

// A client that hangs up mid-request isn't a server fault: report 499 and don't
// log it as an internal error (it would drown real errors during an overload).
func TestClientDisconnectIs499NotInternal(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{Deps: Deps{Log: slog.New(slog.NewJSONHandler(&logs, nil))}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/v1/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.writeError(rec, r, fmt.Errorf("query: %w", context.Canceled))
	assert.Equal(t, 499, rec.Code)
	assert.NotContains(t, logs.String(), "internal error")
}

// Readiness must not depend on the shared connection pool: when the pool is
// saturated under load, every pod's check would fail at once and the load
// balancer would drop all of them (Phase 9 found exactly this). It only fails
// after the database has been unreachable for a sustained period.
func TestReadinessToleratesBriefDBTrouble(t *testing.T) {
	h := NewDBHealth("", 10*time.Second)
	now := time.Now()
	h.now = func() time.Time { return now }
	h.record(nil)
	assert.NoError(t, h.Ready())

	h.record(errors.New("pool exhausted"))
	now = now.Add(9 * time.Second)
	assert.NoError(t, h.Ready(), "brief trouble: stay in the load balancer")

	now = now.Add(2 * time.Second)
	assert.Error(t, h.Ready(), "unreachable for over 10s: take this pod out")

	h.record(nil)
	assert.NoError(t, h.Ready(), "recovers as soon as a check succeeds")
}

// A 503 (payments down, database unreachable) tells the client when to come back,
// so well-behaved clients wait out a circuit breaker's cooldown instead of
// giving up or hammering the API (Phase 9 chaos test).
func TestServiceUnavailableSendsRetryAfter(t *testing.T) {
	s := &Server{Deps: Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	rec := httptest.NewRecorder()
	s.writeError(rec, httptest.NewRequest("POST", "/v1/holds/x/checkout", nil),
		&apperr.Error{Status: 503, Code: "PAYMENT_UNAVAILABLE", Message: "down"})
	assert.Equal(t, 503, rec.Code)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
}
