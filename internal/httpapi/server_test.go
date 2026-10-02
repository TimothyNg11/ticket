package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/account"
	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/cache"
	"ticket/internal/inventory"
	"ticket/internal/payments"
	"ticket/internal/payments/mock"
	"ticket/internal/ratelimit"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// testAdminEmail is registered with the admin role in every test server.
const testAdminEmail = "admin@example.com"

// testEnv is a full API over its own database, with a mock payment provider.
type testEnv struct {
	srv    *httptest.Server
	pool   *pgxpool.Pool
	tokens *auth.TokenIssuer
	pay    *mock.Server
	cache  *cache.Cache
}

func newTestEnv(t *testing.T) *testEnv { return newTestEnvScaled(t, 1000) }

// newTestEnvScaled builds the API with rate limits multiplied by scale.
func newTestEnvScaled(t *testing.T, scale float64) *testEnv {
	t.Helper()
	pool := pg.NewDB(t)
	tokens := auth.NewTokenIssuer(testSecret, 15*time.Minute)
	pay := mock.New(mock.Config{}, nil)
	rdb := testutil.NewRedis(t)
	inv := inventory.New(pool)
	c := cache.New(rdb, inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	paySrv := httptest.NewServer(pay)
	t.Cleanup(paySrv.Close)
	book := booking.New(pool, payments.NewHTTPClient(paySrv.URL, time.Second),
		auth.NewTicketSigner([]byte("ticket-signing-key-ticket-signing-key")), 10*time.Minute)
	book.OnSeatsChanged(c.SeatsChanged)
	h := NewHandler(Deps{
		Pool:      pool,
		Tokens:    tokens,
		Accounts:  account.New(pool, tokens, 24*time.Hour, map[string]bool{testAdminEmail: true}),
		Inventory: inv,
		Booking:   book,
		Cache:     c,
		// Tests send many requests from one IP; scale limits up so only the
		// rate-limit tests (which use their own limiter) ever see a 429.
		Limiter: ratelimit.New(rdb, scale, nil),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, pool: pool, tokens: tokens, pay: pay, cache: c}
}

func newTestServer(t *testing.T) *httptest.Server { return newTestEnv(t).srv }

// call sends a JSON request and returns the status and raw body. Writes get a
// fresh Idempotency-Key unless the test sets one with callWithKey.
func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, []byte) {
	t.Helper()
	return callWithKey(t, srv, method, path, token, uuid.NewString(), body)
}

func callWithKey(t *testing.T, srv *httptest.Server, method, path, token, key string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, r)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

// errCode extracts error.code from an error response body.
func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e), string(body))
	assert.NotEmpty(t, e.Error.RequestID, "every error carries a request id")
	return e.Error.Code
}

func TestHealthAndReady(t *testing.T) {
	srv := newTestServer(t)
	status, body := call(t, srv, "GET", "/healthz", "", nil)
	assert.Equal(t, 200, status)
	assert.JSONEq(t, `{"status":"ok"}`, string(body))

	status, _ = call(t, srv, "GET", "/readyz", "", nil)
	assert.Equal(t, 200, status)
}

func TestMalformedJSONIs400WithErrorShape(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/auth/register", bytes.NewBufferString("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 400, resp.StatusCode)
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}

func TestInvalidPathUUIDIs400(t *testing.T) {
	srv := newTestServer(t)
	status, body := call(t, srv, "GET", "/v1/events/not-a-uuid", "", nil)
	assert.Equal(t, 400, status)
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}
