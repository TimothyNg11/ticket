// Package httpapi is the HTTP layer: routing, middleware, and thin handlers that
// translate between the generated OpenAPI types and the domain services.
package httpapi

//go:generate go tool oapi-codegen -config ../../api/oapi-codegen.yaml -o gen/api.gen.go ../../api/openapi.yaml

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/account"
	"ticket/internal/apperr"
	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/cache"
	"ticket/internal/httpapi/gen"
	"ticket/internal/idempotency"
	"ticket/internal/inventory"
	"ticket/internal/ratelimit"
	"ticket/internal/waitingroom"
)

// Deps are the collaborators the HTTP layer needs.
type Deps struct {
	Pool      *pgxpool.Pool
	Tokens    *auth.TokenIssuer
	Accounts  *account.Service
	Inventory *inventory.Service
	Booking   *booking.Service
	Cache     *cache.Cache
	Room      *waitingroom.Room
	// Draining is set during shutdown; readiness then fails so traffic moves away.
	Draining *atomic.Bool
	Limiter  *ratelimit.Limiter // nil disables rate limiting
	// TrustProxy reads the client IP from X-Forwarded-For (set it only behind a
	// proxy that overwrites that header, like the ingress).
	TrustProxy bool
	Log        *slog.Logger
}

// Server implements gen.StrictServerInterface.
type Server struct {
	Deps
	validate *validator.Validate
}

var _ gen.StrictServerInterface = (*Server)(nil)

// NewHandler builds the full HTTP handler: middleware plus generated routes.
func NewHandler(d Deps) http.Handler {
	s := &Server{Deps: d, validate: validator.New(validator.WithRequiredStructEnabled())}

	r := chi.NewRouter()
	idem := idempotency.New(d.Pool, func(ctx context.Context) (uuid.UUID, bool) {
		c, ok := userFrom(ctx)
		return c.UserID, ok
	}, s.writeError, d.Log)
	r.Use(middleware.RequestID, s.recoverer, limitBody, s.authenticate, s.rateLimit, idem.Handler)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { s.writeError(w, r, apperr.NotFound("route")) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, &apperr.Error{Status: http.StatusMethodNotAllowed, Code: "METHOD_NOT_ALLOWED", Message: "method not allowed"})
	})

	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		// Body could not be decoded (bad JSON, wrong types).
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				s.writeError(w, r, &apperr.Error{Status: http.StatusRequestEntityTooLarge, Code: "REQUEST_TOO_LARGE", Message: "request body exceeds 1 MiB"})
				return
			}
			s.writeError(w, r, apperr.Validation("malformed request body"))
		},
		// A handler returned an error.
		ResponseErrorHandlerFunc: s.writeError,
	})
	return gen.HandlerWithOptions(strict, gen.ChiServerOptions{
		BaseRouter: r,
		// Path or query parameter could not be parsed (e.g. a non-UUID id).
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			s.writeError(w, r, apperr.Validation(err.Error()))
		},
	})
}

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// writeError renders err in the API's single error shape. Unknown errors are
// logged with the request id and reported as a generic 500, so internals never leak.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apperr.Error
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22021" {
		// character_not_in_repertoire: Postgres refuses NUL bytes in text. That is bad
		// client input, so report it as such rather than as a server failure.
		ae = apperr.Validation("text fields must not contain NUL characters")
	} else if !errors.As(err, &ae) {
		s.Log.ErrorContext(r.Context(), "internal error",
			"err", err, "request_id", middleware.GetReqID(r.Context()), "path", r.URL.Path)
		ae = &apperr.Error{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "internal error"}
	}
	var body errorBody
	body.Error.Code = ae.Code
	body.Error.Message = ae.Message
	body.Error.RequestID = middleware.GetReqID(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// check runs struct validation tags on a decoded request body.
func (s *Server) check(v any) error {
	if err := s.validate.Struct(v); err != nil {
		return apperr.Validation(err.Error())
	}
	return nil
}
