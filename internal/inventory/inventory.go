// Package inventory manages venues, events, and seat maps.
package inventory

import (
	"context"
	"encoding/json"
	"time"
	_ "time/tzdata" // embed the IANA database so timezone checks work on minimal images

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/apperr"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
)

// Service implements inventory operations against Postgres.
type Service struct {
	pool *pgxpool.Pool
}

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// RowSpec is one row of seats numbered 1..SeatCount.
type RowSpec struct {
	Label     string
	SeatCount int
}

// SectionSpec is a named section made of rows.
type SectionSpec struct {
	Name string
	Rows []RowSpec
}

// VenueSpec describes a venue and its full seat layout.
type VenueSpec struct {
	Name     string
	Timezone string
	Sections []SectionSpec
}

// SectionResult is a created section and how many seats it has.
type SectionResult struct {
	Section   sqlc.Section
	SeatCount int
}

// VenueResult is a created venue with its sections.
type VenueResult struct {
	Venue    sqlc.Venue
	Sections []SectionResult
}

// CreateVenue creates a venue, its sections, and every physical seat in one
// transaction, so a half-built layout is never visible.
func (s *Service) CreateVenue(ctx context.Context, actor uuid.UUID, spec VenueSpec) (VenueResult, error) {
	if _, err := time.LoadLocation(spec.Timezone); err != nil {
		return VenueResult{}, apperr.Unprocessable("INVALID_TIMEZONE", "unknown IANA timezone: "+spec.Timezone)
	}
	if err := checkUniqueNames(spec); err != nil {
		return VenueResult{}, err
	}
	var out VenueResult
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		v, err := q.CreateVenue(ctx, sqlc.CreateVenueParams{Name: spec.Name, Timezone: spec.Timezone})
		if err != nil {
			return err
		}
		out = VenueResult{Venue: v}
		for _, secSpec := range spec.Sections {
			sec, err := q.CreateSection(ctx, sqlc.CreateSectionParams{VenueID: v.ID, Name: secSpec.Name})
			if err != nil {
				return err
			}
			var seats []sqlc.CreateSeatsParams
			for _, row := range secSpec.Rows {
				for n := 1; n <= row.SeatCount; n++ {
					seats = append(seats, sqlc.CreateSeatsParams{SectionID: sec.ID, RowLabel: row.Label, SeatNumber: int32(n)})
				}
			}
			if _, err := q.CreateSeats(ctx, seats); err != nil {
				return err
			}
			out.Sections = append(out.Sections, SectionResult{Section: sec, SeatCount: len(seats)})
		}
		return audit(ctx, q, actor, "venue.create", "venue:"+v.ID.String(), map[string]any{"name": v.Name})
	})
	return out, err
}

// checkUniqueNames rejects duplicate section names or duplicate row labels within
// a section up front, with a clear error instead of a constraint violation.
func checkUniqueNames(spec VenueSpec) error {
	sections := map[string]bool{}
	for _, sec := range spec.Sections {
		if sections[sec.Name] {
			return apperr.Unprocessable("DUPLICATE_NAME", "duplicate section name: "+sec.Name)
		}
		sections[sec.Name] = true
		rows := map[string]bool{}
		for _, r := range sec.Rows {
			if rows[r.Label] {
				return apperr.Unprocessable("DUPLICATE_NAME", "duplicate row "+r.Label+" in section "+sec.Name)
			}
			rows[r.Label] = true
		}
	}
	return nil
}

// audit records an admin action in the same transaction as the action itself,
// so the log can never disagree with what happened.
func audit(ctx context.Context, q *sqlc.Queries, actor uuid.UUID, action, target string, meta map[string]any) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{ActorID: &actor, Action: action, Target: target, Metadata: b})
}
