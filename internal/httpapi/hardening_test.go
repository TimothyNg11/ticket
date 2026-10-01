package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
