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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/account"
	"ticket/internal/auth"
	"ticket/internal/inventory"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// testAdminEmail is registered with the admin role in every test server.
const testAdminEmail = "admin@example.com"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := pg.NewDB(t)
	tokens := auth.NewTokenIssuer(testSecret, 15*time.Minute)
	h := NewHandler(Deps{
		Pool:      pool,
		Tokens:    tokens,
		Accounts:  account.New(pool, tokens, 24*time.Hour, map[string]bool{testAdminEmail: true}),
		Inventory: inventory.New(pool),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// call sends a JSON request and returns the status and raw body.
func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, []byte) {
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
