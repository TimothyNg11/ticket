package httpapi

import (
	"context"
	"net/http"
	"strings"

	"ticket/internal/apperr"
	"ticket/internal/auth"
)

type claimsKey struct{}

// authenticate verifies a bearer token when one is present and stores its claims
// in the context. A missing header is allowed through (public routes); a present
// but invalid header is rejected outright, so a bad token is never silently ignored.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if h == "" {
			next.ServeHTTP(w, r)
			return
		}
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			s.writeError(w, r, apperr.Unauthenticated("authorization header must be 'Bearer <token>'"))
			return
		}
		c, err := s.Tokens.Verify(tok)
		if err != nil || s.Cache.IsRevoked(r.Context(), c.JTI) {
			s.writeError(w, r, apperr.Unauthenticated("invalid or expired access token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, c)))
	})
}

func userFrom(ctx context.Context) (auth.Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(auth.Claims)
	return c, ok
}

// requireUser returns the caller's claims or a 401.
func requireUser(ctx context.Context) (auth.Claims, error) {
	c, ok := userFrom(ctx)
	if !ok {
		return auth.Claims{}, apperr.Unauthenticated("access token required")
	}
	return c, nil
}

// requireAdmin returns the caller's claims, a 401 if anonymous, or a 403 if not an admin.
func requireAdmin(ctx context.Context) (auth.Claims, error) {
	c, err := requireUser(ctx)
	if err != nil {
		return c, err
	}
	if c.Role != "admin" {
		return c, apperr.Forbidden()
	}
	return c, nil
}
