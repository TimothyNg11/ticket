package inventory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ticket/internal/apperr"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
)

// EventSpec describes a new event. SectionPrices must name every section of the
// venue exactly once; that is the event's price tiers.
type EventSpec struct {
	VenueID       uuid.UUID
	Name          string
	StartsAt      time.Time
	OnSaleAt      time.Time
	SectionPrices map[uuid.UUID]int32
}

// SeatMap is every seat of an event grouped by section, with live state and price.
type SeatMap struct {
	EventID  uuid.UUID
	Sections []SeatMapSection
}

// SeatMapSection is one section of a seat map.
type SeatMapSection struct {
	ID    uuid.UUID
	Name  string
	Seats []SeatMapSeat
}

// SeatMapSeat is one seat of a seat map.
type SeatMapSeat struct {
	EventSeatID uuid.UUID
	Row         string
	Number      int32
	PriceCents  int32
	State       string
}

var errEventNotFound = apperr.NotFound("event")

// CreateEvent creates a draft event and one event_seats row for every seat of the
// venue, priced by section, in a single transaction.
func (s *Service) CreateEvent(ctx context.Context, actor uuid.UUID, spec EventSpec) (sqlc.Event, error) {
	if !spec.OnSaleAt.Before(spec.StartsAt) {
		return sqlc.Event{}, apperr.Unprocessable("INVALID_SCHEDULE", "on_sale_at must be before starts_at")
	}
	var e sqlc.Event
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		if _, err := q.GetVenue(ctx, spec.VenueID); errors.Is(err, pgx.ErrNoRows) {
			return apperr.Unprocessable("UNKNOWN_VENUE", "venue does not exist")
		} else if err != nil {
			return err
		}
		sections, err := q.ListSectionsByVenue(ctx, spec.VenueID)
		if err != nil {
			return err
		}
		if err := checkPrices(sections, spec.SectionPrices); err != nil {
			return err
		}
		e, err = q.CreateEvent(ctx, sqlc.CreateEventParams{
			VenueID: spec.VenueID, Name: spec.Name, StartsAt: spec.StartsAt, OnSaleAt: spec.OnSaleAt,
		})
		if err != nil {
			return err
		}
		for _, sec := range sections {
			if _, err := q.CreateEventSeatsForSection(ctx, sqlc.CreateEventSeatsForSectionParams{
				EventID: e.ID, SectionID: sec.ID, PriceCents: spec.SectionPrices[sec.ID],
			}); err != nil {
				return err
			}
		}
		return audit(ctx, q, actor, "event.create", "event:"+e.ID.String(), map[string]any{"name": e.Name})
	})
	return e, err
}

func checkPrices(sections []sqlc.Section, prices map[uuid.UUID]int32) error {
	own := map[uuid.UUID]bool{}
	for _, sec := range sections {
		own[sec.ID] = true
		if _, ok := prices[sec.ID]; !ok {
			return apperr.Unprocessable("MISSING_SECTION_PRICE", "no price for section "+sec.Name)
		}
	}
	for id := range prices {
		if !own[id] {
			return apperr.Unprocessable("UNKNOWN_SECTION", "section "+id.String()+" is not in this venue")
		}
	}
	return nil
}

// PublishEvent moves a draft event on sale. Only drafts can be published.
func (s *Service) PublishEvent(ctx context.Context, actor, id uuid.UUID) (sqlc.Event, error) {
	var e sqlc.Event
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		var err error
		e, err = q.PublishEvent(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Either it does not exist or it is not a draft; tell them which.
			if _, err := q.GetEvent(ctx, id); errors.Is(err, pgx.ErrNoRows) {
				return errEventNotFound
			}
			return apperr.Conflict("NOT_DRAFT", "only draft events can be published")
		}
		if err != nil {
			return err
		}
		return audit(ctx, q, actor, "event.publish", "event:"+id.String(), map[string]any{})
	})
	return e, err
}

// GetEvent returns a published event. Drafts are reported as not found so their
// existence is not leaked.
func (s *Service) GetEvent(ctx context.Context, id uuid.UUID) (sqlc.Event, error) {
	e, err := sqlc.New(s.pool).GetEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && e.Status == "draft") {
		return sqlc.Event{}, errEventNotFound
	}
	return e, err
}

// ListEvents returns up to limit published events after cursor, ordered by start
// time, and the cursor for the next page ("" on the last page).
func (s *Service) ListEvents(ctx context.Context, cursor string, limit int) ([]sqlc.Event, string, error) {
	limit = min(max(limit, 1), 100)
	afterTS, afterID, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	// Fetch one extra row to learn whether another page exists.
	rows, err := sqlc.New(s.pool).ListPublicEvents(ctx, sqlc.ListPublicEventsParams{
		AfterStartsAt: afterTS, AfterID: afterID, PageSize: int32(limit + 1), //nolint:gosec // clamped to 1..100 above
	})
	if err != nil {
		return nil, "", err
	}
	if len(rows) <= limit {
		return rows, "", nil
	}
	rows = rows[:limit]
	last := rows[len(rows)-1]
	return rows, encodeCursor(last.StartsAt, last.ID), nil
}

// SeatMap returns every seat of a published event with its state and price.
func (s *Service) SeatMap(ctx context.Context, id uuid.UUID) (SeatMap, error) {
	if _, err := s.GetEvent(ctx, id); err != nil {
		return SeatMap{}, err
	}
	rows, err := sqlc.New(s.pool).GetSeatMap(ctx, id)
	if err != nil {
		return SeatMap{}, err
	}
	sm := SeatMap{EventID: id}
	for _, r := range rows {
		// Rows arrive ordered by section, so a new section id starts a new group.
		if n := len(sm.Sections); n == 0 || sm.Sections[n-1].ID != r.SectionID {
			sm.Sections = append(sm.Sections, SeatMapSection{ID: r.SectionID, Name: r.SectionName})
		}
		sec := &sm.Sections[len(sm.Sections)-1]
		sec.Seats = append(sec.Seats, SeatMapSeat{
			EventSeatID: r.EventSeatID, Row: r.RowLabel, Number: r.SeatNumber, PriceCents: r.PriceCents, State: r.State,
		})
	}
	return sm, nil
}

// CountAvailable returns how many of an event's seats are available right now.
func (s *Service) CountAvailable(ctx context.Context, id uuid.UUID) (int, error) {
	n, err := sqlc.New(s.pool).CountAvailableSeats(ctx, id)
	return int(n), err
}

// OnSaleEventIDs lists events currently on sale that haven't started.
func (s *Service) OnSaleEventIDs(ctx context.Context) ([]uuid.UUID, error) {
	return sqlc.New(s.pool).ListOnSaleEventIDs(ctx)
}
