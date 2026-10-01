// Package inventory manages venues, events, and seat maps.
package inventory

import "github.com/jackc/pgx/v5/pgxpool"

// Service implements inventory operations against Postgres.
type Service struct{}

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{} }
