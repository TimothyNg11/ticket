package idempotency_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/apperr"
	"ticket/internal/idempotency"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

type userKey struct{}

// harness wraps a counting handler in the middleware. The caller's user id comes
// from the X-User header, standing in for the real auth middleware.
type harness struct {
	pool  *pgxpool.Pool
	calls atomic.Int64
	srv   *httptest.Server
	// status the inner handler returns; delay simulates slow work.
	status int
	delay  time.Duration
}

func newHarness(t *testing.T) *harness {
	h := &harness{pool: pg.NewDB(t), status: http.StatusCreated}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := h.calls.Add(1)
		time.Sleep(h.delay)
		key, _ := idempotency.KeyFrom(r.Context())
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"call": n, "key": key, "body": string(body)})
	})
	g := idempotency.New(h.pool, func(ctx context.Context) (uuid.UUID, bool) {
		id, ok := ctx.Value(userKey{}).(uuid.UUID)
		return id, ok
	}, func(w http.ResponseWriter, r *http.Request, err error) {
		var ae *apperr.Error
		require.True(t, errors.As(err, &ae))
		w.WriteHeader(ae.Status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": ae.Code})
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	withUser := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u := r.Header.Get("X-User"); u != "" {
				r = r.WithContext(context.WithValue(r.Context(), userKey{}, uuid.MustParse(u)))
			}
			next.ServeHTTP(w, r)
		})
	}
	h.srv = httptest.NewServer(withUser(g.Handler(inner)))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) user(t *testing.T) uuid.UUID {
	return testutil.CreateUser(t, h.pool, uuid.NewString()[:8]+"@x.com", "user")
}

type result struct {
	status int
	body   map[string]any
	replay string
}

func (h *harness) do(t *testing.T, method, path string, user uuid.UUID, key, body string) result {
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if user != uuid.Nil {
		req.Header.Set("X-User", user.String())
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var b map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&b)
	return result{resp.StatusCode, b, resp.Header.Get("Idempotent-Replay")}
}

func TestReplaysStoredResponse(t *testing.T) {
	h := newHarness(t)
	u := h.user(t)
	first := h.do(t, "POST", "/v1/holds/x/checkout", u, "k1", `{"a":1}`)
	second := h.do(t, "POST", "/v1/holds/x/checkout", u, "k1", `{"a":1}`)
	assert.Equal(t, 201, first.status)
	assert.Equal(t, "k1", first.body["key"], "handler sees the key")
	assert.Equal(t, first, result{second.status, second.body, ""}, "same status and body")
	assert.Equal(t, "true", second.replay)
	assert.EqualValues(t, 1, h.calls.Load())
}

func TestDifferentBodySameKeyIs422(t *testing.T) {
	h := newHarness(t)
	u := h.user(t)
	h.do(t, "POST", "/v1/events/e/holds", u, "k1", `{"a":1}`)
	r := h.do(t, "POST", "/v1/events/e/holds", u, "k1", `{"a":2}`)
	assert.Equal(t, 422, r.status)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSED", r.body["code"])
	assert.EqualValues(t, 1, h.calls.Load())
}

func TestMissingOrOverlongKeyIs400(t *testing.T) {
	h := newHarness(t)
	u := h.user(t)
	for _, key := range []string{"", strings.Repeat("k", 65)} {
		r := h.do(t, "POST", "/v1/events/e/holds", u, key, `{}`)
		assert.Equal(t, 400, r.status)
		assert.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", r.body["code"])
	}
	assert.EqualValues(t, 0, h.calls.Load())
}

func TestKeysAreScopedPerUser(t *testing.T) {
	h := newHarness(t)
	a, b := h.user(t), h.user(t)
	h.do(t, "POST", "/v1/events/e/holds", a, "same", `{}`)
	r := h.do(t, "POST", "/v1/events/e/holds", b, "same", `{}`)
	assert.Equal(t, 201, r.status)
	assert.Empty(t, r.replay)
	assert.EqualValues(t, 2, h.calls.Load())
}

func TestConcurrentSameKeyExecutesOnce(t *testing.T) {
	h := newHarness(t)
	h.delay = 200 * time.Millisecond
	u := h.user(t)
	var wg sync.WaitGroup
	statuses := make([]int, 20)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = h.do(t, "POST", "/v1/events/e/holds", u, "k", `{}`).status
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, h.calls.Load())
	for _, s := range statuses {
		assert.Contains(t, []int{201, 409}, s)
	}
}

func Test5xxIsNotStored(t *testing.T) {
	h := newHarness(t)
	h.status = http.StatusServiceUnavailable
	u := h.user(t)
	h.do(t, "POST", "/v1/events/e/holds", u, "k", `{}`)
	h.status = http.StatusCreated
	r := h.do(t, "POST", "/v1/events/e/holds", u, "k", `{}`)
	assert.Equal(t, 201, r.status)
	assert.Empty(t, r.replay)
	assert.EqualValues(t, 2, h.calls.Load())
}

// 499 means the client hung up before the handler finished: not an answer the
// client ever saw, so a retry with the same key must run again.
func TestClientClosedRequestIsNotStored(t *testing.T) {
	h := newHarness(t)
	h.status = 499
	u := h.user(t)
	h.do(t, "POST", "/v1/events/e/holds", u, "k", `{}`)
	h.status = http.StatusCreated
	r := h.do(t, "POST", "/v1/events/e/holds", u, "k", `{}`)
	assert.Equal(t, 201, r.status)
	assert.Empty(t, r.replay)
	assert.EqualValues(t, 2, h.calls.Load())
}

func TestDeleteExpiredRemovesOnlyExpiredKeys(t *testing.T) {
	h := newHarness(t)
	u := h.user(t)
	h.do(t, "POST", "/v1/events/e/holds", u, "old", `{}`)
	h.do(t, "POST", "/v1/events/e/holds", u, "new", `{}`)
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = 'old'`)
	require.NoError(t, err)

	n, err := idempotency.DeleteExpired(ctx, h.pool)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	var keys []string
	rows, err := h.pool.Query(ctx, `SELECT key FROM idempotency_keys`)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	assert.Equal(t, []string{"new"}, keys)
}

func TestAbandonedInProgressIsTakenOver(t *testing.T) {
	h := newHarness(t)
	u := h.user(t)
	ctx := context.Background()
	// A request that died mid-flight two minutes ago left its claim behind.
	_, err := h.pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, request_hash, created_at, expires_at)
		VALUES ($1, 'k', $2, now() - interval '2 minutes', now() + interval '1 day')`,
		u, idempotency.RequestHash("POST", "/v1/events/e/holds", []byte(`{}`)))
	require.NoError(t, err)
	r := h.do(t, "POST", "/v1/events/e/holds", u, "k", `{}`)
	assert.Equal(t, 201, r.status)
	assert.EqualValues(t, 1, h.calls.Load())

	// A fresh in-progress claim is respected instead.
	_, err = h.pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, request_hash, expires_at)
		VALUES ($1, 'k2', $2, now() + interval '1 day')`, u, idempotency.RequestHash("POST", "/v1/events/e/holds", []byte(`{}`)))
	require.NoError(t, err)
	r = h.do(t, "POST", "/v1/events/e/holds", u, "k2", `{}`)
	assert.Equal(t, 409, r.status)
	assert.Equal(t, "REQUEST_IN_PROGRESS", r.body["code"])
}

func TestSkipsAuthAdminGETAndAnonymous(t *testing.T) {
	h := newHarness(t)
	u := h.user(t)
	for _, c := range []struct {
		method, path string
		user         uuid.UUID
	}{
		{"POST", "/v1/auth/login", u},
		{"POST", "/v1/admin/venues", u},
		{"GET", "/v1/orders", u},
		{"POST", "/v1/events/e/holds", uuid.Nil}, // anonymous: let the handler 401
		// Joining a queue is idempotent by construction (ZADD NX keeps your
		// place), so it skips the two Postgres writes per request (Phase 9).
		{"POST", "/v1/events/e/queue", u},
	} {
		r := h.do(t, c.method, c.path, c.user, "", `{}`)
		assert.Equal(t, 201, r.status, c.path)
	}
	assert.EqualValues(t, 5, h.calls.Load())
}
