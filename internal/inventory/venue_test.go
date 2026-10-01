package inventory_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/apperr"
	"ticket/internal/inventory"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

func code(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func smallVenue() inventory.VenueSpec {
	return inventory.VenueSpec{
		Name: "Hall", Timezone: "America/New_York",
		Sections: []inventory.SectionSpec{
			{Name: "Floor", Rows: []inventory.RowSpec{{Label: "A", SeatCount: 3}, {Label: "B", SeatCount: 2}}},
			{Name: "Balcony", Rows: []inventory.RowSpec{{Label: "A", SeatCount: 4}}},
		},
	}
}

func TestCreateVenue(t *testing.T) {
	pool := pg.NewDB(t)
	s := inventory.New(pool)
	admin := testutil.CreateUser(t, pool, "admin@example.com", "admin")

	v, err := s.CreateVenue(context.Background(), admin, smallVenue())
	require.NoError(t, err)
	assert.Equal(t, "Hall", v.Venue.Name)
	require.Len(t, v.Sections, 2)
	counts := map[string]int{}
	for _, sec := range v.Sections {
		counts[sec.Section.Name] = sec.SeatCount
	}
	assert.Equal(t, map[string]int{"Floor": 5, "Balcony": 4}, counts)

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM seats").Scan(&n))
	assert.Equal(t, 9, n)
	require.NoError(t, pool.QueryRow(context.Background(),
		"SELECT count(*) FROM audit_log WHERE action = 'venue.create' AND actor_id = $1", admin).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestCreateVenueRejectsBadTimezone(t *testing.T) {
	pool := pg.NewDB(t)
	spec := smallVenue()
	spec.Timezone = "Mars/Olympus_Mons"
	_, err := inventory.New(pool).CreateVenue(context.Background(), testutil.CreateUser(t, pool, "a@x.com", "admin"), spec)
	assert.Equal(t, "INVALID_TIMEZONE", code(err))
}

func TestCreateVenueRejectsDuplicateNames(t *testing.T) {
	pool := pg.NewDB(t)
	admin := testutil.CreateUser(t, pool, "a@x.com", "admin")
	s := inventory.New(pool)

	dupSection := smallVenue()
	dupSection.Sections[1].Name = "Floor"
	_, err := s.CreateVenue(context.Background(), admin, dupSection)
	assert.Equal(t, "DUPLICATE_NAME", code(err))

	dupRow := smallVenue()
	dupRow.Sections[0].Rows[1].Label = "A"
	_, err = s.CreateVenue(context.Background(), admin, dupRow)
	assert.Equal(t, "DUPLICATE_NAME", code(err))
}
