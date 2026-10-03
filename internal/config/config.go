// Package config loads API settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting the API reads from the environment.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// TicketSigningKey signs ticket QR payloads. Separate from JWTSecret so the
	// two can be rotated independently.
	TicketSigningKey []byte
	PaymentsURL      string
	HoldTTL          time.Duration
	RedisURL         string
	// RateLimitScale multiplies every rate limit (1 in production; load tests
	// that simulate many users from few machines raise it).
	RateLimitScale float64
	// TrustProxy reads client IPs from X-Forwarded-For. Enable only behind an
	// ingress that sets that header.
	TrustProxy bool
	// DrainDelay is how long the API keeps serving after SIGTERM with readiness
	// failing, so load balancers stop routing to it before it stops listening.
	DrainDelay time.Duration
	// HashConcurrency caps simultaneous Argon2id computations (19 MiB each).
	HashConcurrency int
	// AdminEmails lists (lower-cased) emails that receive the admin role when they
	// register. It is how the first admin is bootstrapped; see docs/decisions/0002.
	AdminEmails map[string]bool
}

// Load reads configuration through getenv (normally os.Getenv). Taking the lookup
// function as a parameter keeps Load testable without mutating the process environment.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		HTTPAddr:         getenv("HTTP_ADDR"),
		DatabaseURL:      getenv("DATABASE_URL"),
		JWTSecret:        []byte(getenv("JWT_SECRET")),
		TicketSigningKey: []byte(getenv("TICKET_SIGNING_KEY")),
		PaymentsURL:      getenv("PAYMENTS_URL"),
		RedisURL:         getenv("REDIS_URL"),
		TrustProxy:       getenv("TRUST_PROXY") == "true",
		RateLimitScale:   1,
		HashConcurrency:  4,
		AdminEmails:      map[string]bool{},
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	// HS256 is only as strong as its key; 32 bytes matches the hash output size.
	if len(c.JWTSecret) < 32 {
		return Config{}, errors.New("JWT_SECRET must be at least 32 bytes")
	}
	if len(c.TicketSigningKey) < 32 {
		return Config{}, errors.New("TICKET_SIGNING_KEY must be at least 32 bytes")
	}
	if c.PaymentsURL == "" {
		return Config{}, errors.New("PAYMENTS_URL is required")
	}
	if c.RedisURL == "" {
		return Config{}, errors.New("REDIS_URL is required")
	}
	if v := getenv("ARGON2_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("ARGON2_CONCURRENCY must be a positive integer, got %q", v)
		}
		c.HashConcurrency = n
	}
	if v := getenv("RATE_LIMIT_SCALE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return Config{}, fmt.Errorf("RATE_LIMIT_SCALE must be a positive number, got %q", v)
		}
		c.RateLimitScale = f
	}
	var err error
	if c.HoldTTL, err = duration(getenv, "HOLD_TTL", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if c.DrainDelay, err = duration(getenv, "DRAIN_DELAY", 5*time.Second); err != nil {
		return Config{}, err
	}
	if c.AccessTokenTTL, err = duration(getenv, "ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if c.RefreshTokenTTL, err = duration(getenv, "REFRESH_TOKEN_TTL", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	for _, e := range strings.Split(getenv("ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			c.AdminEmails[e] = true
		}
	}
	return c, nil
}

func duration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
