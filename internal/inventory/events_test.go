package inventory_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/inventory"
	"ticket/internal/testutil"
)

type fixture struct {
	s      *inventory.Service
	admin  uuid.UUID
	venue  inventory.VenueResult
	prices map[uuid.UUID]int32
}

func setup(t *testing.T) fixture {
	pool := pg.NewDB(t)
	s := inventory.New(pool)
	admin := testutil.CreateUser(t, pool, "admin@example.com", "admin")
	v, err := s.CreateVenue(context.Background(), admin, smallVenue())
	require.NoError(t, err)
	prices := map[uuid.UUID]int32{}
	for i, sec := range v.Sections {
		prices[sec.Section.ID] = int32(1000 * (i + 1))
	}
	return fixture{s: s, admin: admin, venue: v, prices: prices}
}

func (f fixture) spec(name string, startsIn time.Duration) inventory.EventSpec {
	now := time.Now()
	return inventory.EventSpec{
		VenueID: f.venue.Venue.ID, Name: name,
		StartsAt: now.Add(startsIn), OnSaleAt: now.Add(-time.Hour), SectionPrices: f.prices,
	}
}

func TestCreateEventCreatesEventSeats(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e, err := f.s.CreateEvent(ctx, f.admin, f.spec("Show", 48*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, "draft", e.Status)

	// Drafts are invisible publicly.
	_, err = f.s.GetEvent(ctx, e.ID)
	assert.Equal(t, "NOT_FOUND", code(err))
	_, err = f.s.SeatMap(ctx, e.ID)
	assert.Equal(t, "NOT_FOUND", code(err))

	_, err = f.s.PublishEvent(ctx, f.admin, e.ID)
	require.NoError(t, err)
	_, err = f.s.PublishEvent(ctx, f.admin, e.ID)
	assert.Equal(t, "NOT_DRAFT", code(err), "publishing twice is a conflict")
	_, err = f.s.PublishEvent(ctx, f.admin, uuid.New())
	assert.Equal(t, "NOT_FOUND", code(err))

	sm, err := f.s.SeatMap(ctx, e.ID)
	require.NoError(t, err)
	require.Len(t, sm.Sections, 2)
	total := 0
	for _, sec := range sm.Sections {
		for _, seat := range sec.Seats {
			assert.Equal(t, "available", seat.State)
			assert.Equal(t, f.prices[sec.ID], seat.PriceCents)
			total++
		}
	}
	assert.Equal(t, 9, total)
	// Sections sort by name (Balcony, Floor); seats by row then number.
	floor := sm.Sections[1]
	assert.Equal(t, "Floor", floor.Name)
	assert.Equal(t, "A", floor.Seats[0].Row)
	assert.Equal(t, int32(1), floor.Seats[0].Number)
}

func TestCreateEventPricingRules(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	missing := f.spec("x", 48*time.Hour)
	missing.SectionPrices = map[uuid.UUID]int32{f.venue.Sections[0].Section.ID: 500}
	_, err := f.s.CreateEvent(ctx, f.admin, missing)
	assert.Equal(t, "MISSING_SECTION_PRICE", code(err))

	other, err := f.s.CreateVenue(ctx, f.admin, smallVenue())
	require.NoError(t, err)
	foreign := f.spec("x", 48*time.Hour)
	foreign.SectionPrices = map[uuid.UUID]int32{}
	for k, v := range f.prices {
		foreign.SectionPrices[k] = v
	}
	foreign.SectionPrices[other.Sections[0].Section.ID] = 500
	_, err = f.s.CreateEvent(ctx, f.admin, foreign)
	assert.Equal(t, "UNKNOWN_SECTION", code(err))

	unknownVenue := f.spec("x", 48*time.Hour)
	unknownVenue.VenueID = uuid.New()
	_, err = f.s.CreateEvent(ctx, f.admin, unknownVenue)
	assert.Equal(t, "UNKNOWN_VENUE", code(err))

	badSchedule := f.spec("x", 48*time.Hour)
	badSchedule.OnSaleAt = badSchedule.StartsAt.Add(time.Minute)
	_, err = f.s.CreateEvent(ctx, f.admin, badSchedule)
	assert.Equal(t, "INVALID_SCHEDULE", code(err))
}

func TestListEventsPaginates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		e, err := f.s.CreateEvent(ctx, f.admin, f.spec("E", time.Duration(i)*24*time.Hour))
		require.NoError(t, err)
		_, err = f.s.PublishEvent(ctx, f.admin, e.ID)
		require.NoError(t, err)
	}
	_, err := f.s.CreateEvent(ctx, f.admin, f.spec("draft", 12*time.Hour)) // never published
	require.NoError(t, err)

	var seen []uuid.UUID
	cursor := ""
	pages := 0
	for {
		page, next, err := f.s.ListEvents(ctx, cursor, 2)
		require.NoError(t, err)
		pages++
		for _, e := range page {
			seen = append(seen, e.ID)
			assert.NotEqual(t, "draft", e.Status)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	assert.Len(t, seen, 5)
	assert.Equal(t, 3, pages)

	page, _, err := f.s.ListEvents(ctx, "", 0)
	require.NoError(t, err, "a non-positive limit is clamped, not a panic")
	assert.Len(t, page, 1)
}
