package httpapi

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"ticket/internal/apperr"
	"ticket/internal/metrics"
	"ticket/internal/ratelimit"
)

// Rate-limit rules. Writes are limited per user, anonymous traffic per client IP.
// Login and registration are limited per IP to slow credential stuffing and
// account farming; holds are tight because each one locks inventory.
var (
	ruleLogin    = ratelimit.PerMinute("login", 10)
	ruleRegister = ratelimit.PerMinute("register", 5)
	ruleHolds    = ratelimit.PerMinute("holds", 5)
	ruleQueue    = ratelimit.PerMinute("queue", 6)
	ruleWrites   = ratelimit.PerMinute("writes", 60)
	ruleReads    = ratelimit.Rule{Name: "reads", Rate: 20, Burst: 40}
)

// limitFor picks the bucket for a request. ok is false for unlimited routes.
func (s *Server) limitFor(r *http.Request) (rule ratelimit.Rule, key string, ok bool) {
	p := r.URL.Path
	ip := s.clientIP(r)
	user, authed := userFrom(r.Context())
	who := "ip:" + ip
	if authed {
		who = "user:" + user.UserID.String()
	}
	switch {
	case p == "/healthz" || p == "/readyz" || p == "/metrics":
		return rule, "", false
	case r.Method == http.MethodGet:
		return ruleReads, who, true
	case p == "/v1/auth/login":
		return ruleLogin, "ip:" + ip, true
	case p == "/v1/auth/register":
		return ruleRegister, "ip:" + ip, true
	case strings.HasPrefix(p, "/v1/events/") && strings.HasSuffix(p, "/holds"):
		return ruleHolds, who, true
	case strings.HasPrefix(p, "/v1/events/") && strings.HasSuffix(p, "/queue"):
		return ruleQueue, who, true
	default:
		return ruleWrites, who, true
	}
}

// rateLimit rejects requests over their bucket with 429 and a Retry-After header.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rule, key, ok := s.limitFor(r)
		if !ok || s.Limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		d := s.Limiter.Allow(r.Context(), rule, key)
		if !d.Allowed {
			metrics.RateLimited.WithLabelValues(rule.Name).Inc()
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
			s.writeError(w, r, &apperr.Error{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED",
				Message: "too many requests; retry after the time in the Retry-After header"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's address. Behind the ingress (TrustProxy), the
// ingress appends the address it saw to X-Forwarded-For, so the right-most entry
// is the one a client can't forge; anything to its left is client-supplied.
func (s *Server) clientIP(r *http.Request) string {
	if s.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
