// Package config loads API settings from environment variables.
package config

import (
	"errors"
	"fmt"
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
	// AdminEmails lists (lower-cased) emails that receive the admin role when they
	// register. It is how the first admin is bootstrapped; see docs/decisions/0002.
	AdminEmails map[string]bool
}

// Load reads configuration through getenv (normally os.Getenv). Taking the lookup
// function as a parameter keeps Load testable without mutating the process environment.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		HTTPAddr:    getenv("HTTP_ADDR"),
		DatabaseURL: getenv("DATABASE_URL"),
		JWTSecret:   []byte(getenv("JWT_SECRET")),
		AdminEmails: map[string]bool{},
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
	var err error
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
