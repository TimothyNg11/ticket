// Package idempotency makes retried write requests safe. A client sends an
// Idempotency-Key header; the first request with that key runs and its response is
// stored, and any retry with the same key gets the stored response instead of
// running again. This is what lets a client whose connection dropped mid-checkout
// simply retry without risking a second charge.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/apperr"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
	"ticket/internal/metrics"
)

const (
	// Header is the request header carrying the client's key.
	Header = "Idempotency-Key"
	// ttl is how long a key and its response are remembered (spec: 24 hours).
	ttl = 24 * time.Hour
	// staleAfter is how long an in-progress claim is respected. A claim older than
	// this belongs to a request that crashed before saving its response.
	staleAfter = time.Minute
	maxKeyLen  = 64
)

var (
	errKeyRequired = &apperr.Error{Status: http.StatusBadRequest, Code: "IDEMPOTENCY_KEY_REQUIRED",
		Message: "this request requires an Idempotency-Key header of 1 to 64 characters"}
	errKeyReused  = apperr.Unprocessable("IDEMPOTENCY_KEY_REUSED", "this Idempotency-Key was already used with a different request")
	errInProgress = apperr.Conflict("REQUEST_IN_PROGRESS", "a request with this Idempotency-Key is still being processed; retry shortly")
)

type keyCtx struct{}

// KeyFrom returns the request's validated Idempotency-Key, if the middleware ran.
func KeyFrom(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(keyCtx{}).(string)
	return k, ok
}

// RequestHash fingerprints a request so a reused key with a different request is caught.
func RequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method + " " + path + "\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Guard is the idempotency middleware.
type Guard struct {
	pool     *pgxpool.Pool
	userFrom func(context.Context) (uuid.UUID, bool)
	onError  func(http.ResponseWriter, *http.Request, error)
	log      *slog.Logger
}

// New returns a Guard. userFrom reports the authenticated caller; onError renders
// errors in the API's error shape.
func New(pool *pgxpool.Pool, userFrom func(context.Context) (uuid.UUID, bool),
	onError func(http.ResponseWriter, *http.Request, error), log *slog.Logger) *Guard {
	return &Guard{pool: pool, userFrom: userFrom, onError: onError, log: log}
}

// applies reports whether a request needs a key: authenticated user writes under
// /v1. Auth endpoints have no user yet, admin endpoints are operator tools, and
// reads are naturally idempotent. Joining a waiting room is idempotent by
// construction (rejoining keeps your place), so it skips the two Postgres writes
// this middleware costs; at 50,000 joins in 30 s those writes were the database's
// biggest load (Phase 9, ADR 0009).
func applies(r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1/events/") && strings.HasSuffix(p, "/queue") {
		return false
	}
	return strings.HasPrefix(p, "/v1/") && !strings.HasPrefix(p, "/v1/auth/") && !strings.HasPrefix(p, "/v1/admin/")
}

// Handler wraps next with idempotency checks.
func (g *Guard) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := g.userFrom(r.Context())
		if !applies(r) || !ok {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get(Header)
		if key == "" || len(key) > maxKeyLen {
			g.onError(w, r, errKeyRequired)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			g.onError(w, r, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		hash := RequestHash(r.Method, r.URL.Path, body)
		ctx := r.Context()
		q := sqlc.New(g.pool)

		var claimed int64
		err = db.InAsyncCommitTx(ctx, g.pool, func(q *sqlc.Queries) error {
			var err error
			claimed, err = q.ClaimIdempotencyKey(ctx, sqlc.ClaimIdempotencyKeyParams{
				UserID: user, Key: key, RequestHash: hash, ExpiresAt: time.Now().Add(ttl),
			})
			return err
		})
		if err != nil {
			g.onError(w, r, err)
			return
		}
		if claimed == 0 {
			existing, err := q.GetIdempotencyKey(ctx, sqlc.GetIdempotencyKeyParams{UserID: user, Key: key})
			if errors.Is(err, pgx.ErrNoRows) {
				// The first request failed with a 5xx and released the key between our
				// claim attempt and this read; the client should simply retry.
				g.onError(w, r, errInProgress)
				return
			}
			if err != nil {
				g.onError(w, r, err)
				return
			}
			switch {
			case existing.RequestHash != hash:
				g.onError(w, r, errKeyReused)
				return
			case existing.ResponseStatus != nil:
				replay(w, int(*existing.ResponseStatus), existing.ResponseBody)
				return
			}
			took, err := q.TakeOverIdempotencyKey(ctx, sqlc.TakeOverIdempotencyKeyParams{
				UserID: user, Key: key, Cutoff: time.Now().Add(-staleAfter),
			})
			if err != nil {
				g.onError(w, r, err)
				return
			}
			if took == 0 {
				g.onError(w, r, errInProgress)
				return
			}
			g.log.WarnContext(ctx, "took over abandoned idempotency key", "user_id", user, "key", key)
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(ctx, keyCtx{}, key)))

		// Save with a fresh context: the response is already written, and losing the
		// record because the client hung up would let a retry run twice.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = db.InAsyncCommitTx(saveCtx, g.pool, func(q *sqlc.Queries) error {
			if rec.status >= 500 || rec.status == 499 {
				// Server-side failures and requests the client abandoned (499) aren't
				// final answers; free the key so a retry runs.
				return q.DeleteIdempotencyKey(saveCtx, sqlc.DeleteIdempotencyKeyParams{UserID: user, Key: key})
			}
			return q.SaveIdempotentResponse(saveCtx, sqlc.SaveIdempotentResponseParams{
				UserID: user, Key: key, ResponseStatus: ptr(statusCode(rec.status)), ResponseBody: rec.body.Bytes(),
			})
		})
		if err != nil {
			g.log.ErrorContext(ctx, "saving idempotent response", "err", err, "user_id", user, "key", key)
		}
	})
}

// DeleteExpired removes keys past their 24-hour lifetime and returns how many it
// removed. Every write request adds one, so something must run this regularly.
func DeleteExpired(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	return sqlc.New(pool).DeleteExpiredIdempotencyKeys(ctx)
}

func replay(w http.ResponseWriter, status int, body []byte) {
	metrics.IdempotentReplays.Inc()
	if len(body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// recorder passes the response through while keeping a copy to store.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func ptr[T any](v T) *T { return &v }

// statusCode narrows an HTTP status (always 100..999) to the column type.
func statusCode(s int) int32 { return int32(s) } //nolint:gosec // HTTP status codes are < 1000
