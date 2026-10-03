package observability

import (
	"strings"

	"github.com/exaring/otelpgx"
)

// QuerySpanName names a database span after the sqlc query that produced the
// SQL ("-- name: GetHoldForUpdate :one" becomes "db GetHoldForUpdate"), so
// traces read like the code. Hand-written SQL falls back to its first keyword.
func QuerySpanName(sql string) string {
	sql = strings.TrimSpace(sql)
	if rest, ok := strings.CutPrefix(sql, "-- name:"); ok {
		if f := strings.Fields(rest); len(f) > 0 {
			return "db " + f[0]
		}
	}
	if f := strings.Fields(sql); len(f) > 0 {
		return "db " + strings.ToUpper(f[0])
	}
	return "db"
}

// NewDBTracer returns the pgx tracer every binary installs.
func NewDBTracer() *otelpgx.Tracer {
	return otelpgx.NewTracer(otelpgx.WithSpanNameFunc(QuerySpanName))
}
