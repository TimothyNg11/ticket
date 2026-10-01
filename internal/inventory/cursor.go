package inventory

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticket/internal/apperr"
)

// Cursors are opaque to clients (base64) so the encoding can change without an API change.
// They hold the sort key of the last item returned: (starts_at, id).
func encodeCursor(startsAt time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(startsAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

// decodeCursor returns the zero time and uuid.Nil for an empty cursor, which sorts
// before every real event.
func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	if c == "" {
		return time.Time{}, uuid.Nil, nil
	}
	bad := apperr.Validation("invalid cursor")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	tsPart, idPart, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, bad
	}
	ts, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	return ts, id, nil
}
