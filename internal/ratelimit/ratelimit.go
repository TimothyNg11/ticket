// Package ratelimit implements token-bucket rate limits shared by every API pod.
//
// A bucket holds up to Burst tokens and refills at Rate tokens per second; each
// request spends one. The check-and-spend runs as a single Lua script inside
// Redis, so two concurrent requests can never both spend the last token, which a
// read-then-write from Go could not guarantee.
package ratelimit

import (
	"context"
	_ "embed"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// Rule is a token-bucket configuration.
type Rule struct {
	Name  string  // bucket family, part of the key, e.g. "login"
	Rate  float64 // tokens added per second
	Burst int     // bucket capacity
}

// PerMinute builds a Rule allowing n requests per minute with a burst of n.
func PerMinute(name string, n int) Rule {
	return Rule{Name: name, Rate: float64(n) / 60, Burst: n}
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until a token is available (zero when allowed).
	RetryAfter time.Duration
}

//go:embed bucket.lua
var bucketLua string

var bucketScript = redis.NewScript(bucketLua)

// Limiter checks rules in Redis, falling back to a stricter in-process limiter
// when Redis is unreachable so an outage degrades protection instead of removing it.
type Limiter struct {
	rdb      *redis.Client
	scale    float64 // multiplies every rule's rate and burst (load tests raise it)
	fallback *local
	onError  func(error)
}

// New returns a Limiter. scale multiplies every rule (1 for production).
func New(rdb *redis.Client, scale float64, onError func(error)) *Limiter {
	if scale <= 0 {
		scale = 1
	}
	if onError == nil {
		onError = func(error) {}
	}
	return &Limiter{rdb: rdb, scale: scale, fallback: newLocal(), onError: onError}
}

// Allow spends one token from the bucket (rule, key).
func (l *Limiter) Allow(ctx context.Context, r Rule, key string) Decision {
	r.Rate *= l.scale
	r.Burst = int(math.Max(1, math.Round(float64(r.Burst)*l.scale)))
	res, err := bucketScript.Run(ctx, l.rdb, []string{"rl:" + r.Name + ":" + key}, r.Rate, r.Burst).Int64Slice()
	if err != nil {
		l.onError(err)
		// Each pod now limits on its own, so quarter the allowance to keep the
		// fleet-wide total roughly in check.
		return l.fallback.allow(Rule{Name: r.Name, Rate: r.Rate / 4, Burst: max(1, r.Burst/4)}, key)
	}
	if res[0] == 1 {
		return Decision{Allowed: true}
	}
	return Decision{RetryAfter: time.Duration(res[1]) * time.Millisecond}
}

// local is the in-process fallback: one golang.org/x/time/rate limiter per key.
type local struct {
	mu   sync.Mutex
	lims map[string]*rate.Limiter
}

func newLocal() *local { return &local{lims: map[string]*rate.Limiter{}} }

func (l *local) allow(r Rule, key string) Decision {
	l.mu.Lock()
	k := r.Name + ":" + key
	lim, ok := l.lims[k]
	if !ok {
		if len(l.lims) > 100_000 { // bound memory during a long outage
			l.lims = map[string]*rate.Limiter{}
		}
		lim = rate.NewLimiter(rate.Limit(r.Rate), r.Burst)
		l.lims[k] = lim
	}
	l.mu.Unlock()
	res := lim.Reserve()
	if d := res.Delay(); d > 0 {
		res.Cancel()
		return Decision{RetryAfter: d}
	}
	return Decision{Allowed: true}
}
