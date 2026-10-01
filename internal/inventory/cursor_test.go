package inventory

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 1, 12, 30, 0, 123456000, time.UTC)
	id := uuid.New()
	gotTS, gotID, err := decodeCursor(encodeCursor(ts, id))
	require.NoError(t, err)
	assert.True(t, ts.Equal(gotTS))
	assert.Equal(t, id, gotID)
}

func TestEmptyCursorStartsAtBeginning(t *testing.T) {
	ts, id, err := decodeCursor("")
	require.NoError(t, err)
	assert.True(t, ts.IsZero())
	assert.Equal(t, uuid.Nil, id)
}

func TestBadCursor(t *testing.T) {
	for _, c := range []string{"!!!", "bm9waXBl", encodeCursor(time.Now(), uuid.New())[:5]} {
		_, _, err := decodeCursor(c)
		assert.Error(t, err, c)
	}
}
