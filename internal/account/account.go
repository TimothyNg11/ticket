// Package account implements registration, login, and token refresh.
package account

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/auth"
)

// Service implements account operations against Postgres.
type Service struct{}

// New returns a Service.
func New(pool *pgxpool.Pool, tokens *auth.TokenIssuer, refreshTTL time.Duration, adminEmails map[string]bool) *Service {
	return &Service{}
}
