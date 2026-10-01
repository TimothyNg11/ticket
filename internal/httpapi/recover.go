package httpapi

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5/middleware"
)

// maxBodyBytes caps request bodies. The largest legitimate payload (an admin venue
// layout) is tens of kilobytes; without a cap, one huge POST could exhaust memory.
const maxBodyBytes = 1 << 20

// limitBody makes reads past maxBodyBytes fail with *http.MaxBytesError.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// recoverer turns a panic into a logged, JSON-shaped 500 instead of a dropped
// connection or a plain-text page.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v) // deliberate abort; let net/http handle it
				}
				s.Log.ErrorContext(r.Context(), "panic", "value", v,
					"request_id", middleware.GetReqID(r.Context()), "stack", string(debug.Stack()))
				s.writeError(w, r, fmt.Errorf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
