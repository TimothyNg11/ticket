package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publishedEvent creates a venue and a published event as admin and returns the event id.
func publishedEvent(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	admin := registerAndLogin(t, srv, testAdminEmail)
	status, body := call(t, srv, "POST", "/v1/admin/venues", admin.AccessToken, venueBody())
	require.Equal(t, 201, status, string(body))
	var v struct {
		ID       string `json:"id"`
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	require.NoError(t, json.Unmarshal(body, &v))

	status, body = call(t, srv, "POST", "/v1/admin/events", admin.AccessToken, map[string]any{
		"venue_id": v.ID, "name": "Show",
		"starts_at":      time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		"on_sale_at":     time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"section_prices": []map[string]any{{"section_id": v.Sections[0].ID, "price_cents": 5000}},
	})
	require.Equal(t, 201, status, string(body))
	var e struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &e))

	// Draft is hidden until published.
	status, _ = call(t, srv, "GET", "/v1/events/"+e.ID, "", nil)
	require.Equal(t, 404, status)

	status, body = call(t, srv, "POST", "/v1/admin/events/"+e.ID+"/publish", admin.AccessToken, nil)
	require.Equal(t, 200, status, string(body))
	return e.ID
}

func TestPublicEventRoutes(t *testing.T) {
	srv := newTestServer(t)
	id := publishedEvent(t, srv)

	status, body := call(t, srv, "GET", "/v1/events", "", nil)
	require.Equal(t, 200, status)
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, id, page.Items[0].ID)
	assert.Nil(t, page.NextCursor)

	status, _ = call(t, srv, "GET", "/v1/events/"+id, "", nil)
	assert.Equal(t, 200, status)

	status, body = call(t, srv, "GET", "/v1/events/"+id+"/seatmap", "", nil)
	require.Equal(t, 200, status)
	var sm struct {
		Sections []struct {
			Seats []struct {
				State      string `json:"state"`
				PriceCents int    `json:"price_cents"`
			} `json:"seats"`
		} `json:"sections"`
	}
	require.NoError(t, json.Unmarshal(body, &sm))
	assert.Len(t, sm.Sections[0].Seats, 5)
	assert.Equal(t, 5000, sm.Sections[0].Seats[0].PriceCents)
}

func TestAdminCreateEventRejectsDuplicateSectionPrice(t *testing.T) {
	srv := newTestServer(t)
	admin := registerAndLogin(t, srv, testAdminEmail)
	status, body := call(t, srv, "POST", "/v1/admin/venues", admin.AccessToken, venueBody())
	require.Equal(t, 201, status)
	var v struct {
		ID       string `json:"id"`
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	price := map[string]any{"section_id": v.Sections[0].ID, "price_cents": 100}
	status, body = call(t, srv, "POST", "/v1/admin/events", admin.AccessToken, map[string]any{
		"venue_id": v.ID, "name": "Show",
		"starts_at":      time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		"on_sale_at":     time.Now().UTC().Format(time.RFC3339),
		"section_prices": []map[string]any{price, price},
	})
	assert.Equal(t, 400, status)
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}

func TestBadCursorIs400(t *testing.T) {
	srv := newTestServer(t)
	status, body := call(t, srv, "GET", "/v1/events?cursor=!!!", "", nil)
	assert.Equal(t, 400, status)
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}

func TestUnknownEventIs404(t *testing.T) {
	srv := newTestServer(t)
	status, body := call(t, srv, "GET", "/v1/events/00000000-0000-0000-0000-000000000001/seatmap", "", nil)
	assert.Equal(t, 404, status)
	assert.Equal(t, "NOT_FOUND", errCode(t, body))
}
