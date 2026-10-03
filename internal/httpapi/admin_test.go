package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/auth"
)

func venueBody() map[string]any {
	return map[string]any{
		"name": "Hall", "timezone": "UTC",
		"sections": []map[string]any{
			{"name": "Floor", "rows": []map[string]any{{"label": "A", "seat_count": 3}, {"label": "B", "seat_count": 2}}},
		},
	}
}

func TestAdminCreateVenue(t *testing.T) {
	srv := newTestServer(t)
	admin := registerAndLogin(t, srv, testAdminEmail)

	status, body := call(t, srv, "POST", "/v1/admin/venues", admin.AccessToken, venueBody())
	require.Equal(t, 201, status, string(body))
	var v struct {
		ID       string `json:"id"`
		Sections []struct {
			SeatCount int `json:"seat_count"`
		} `json:"sections"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	assert.Equal(t, 5, v.Sections[0].SeatCount)
}

func TestAdminRoutesRequireAdmin(t *testing.T) {
	srv := newTestServer(t)
	user := registerAndLogin(t, srv, "user@example.com")

	status, body := call(t, srv, "POST", "/v1/admin/venues", "", venueBody())
	assert.Equal(t, 401, status)
	assert.Equal(t, "UNAUTHENTICATED", errCode(t, body))

	status, body = call(t, srv, "POST", "/v1/admin/venues", user.AccessToken, venueBody())
	assert.Equal(t, 403, status)
	assert.Equal(t, "FORBIDDEN", errCode(t, body))
}

func TestForgedTokensAre401(t *testing.T) {
	srv := newTestServer(t)
	other, _ := auth.NewTokenIssuer([]byte("a-completely-different-secret-xx"), time.Minute).Issue(uuid.New(), "admin")
	expired, _ := auth.NewTokenIssuer(testSecret, -time.Minute).Issue(uuid.New(), "admin")
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": uuid.NewString(), "role": "admin", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	for name, tok := range map[string]string{"wrong secret": other, "expired": expired, "alg none": none, "garbage": "x.y.z"} {
		t.Run(name, func(t *testing.T) {
			// Even a public route rejects a present-but-invalid token rather than
			// silently treating the caller as anonymous.
			status, body := call(t, srv, "GET", "/v1/events", tok, nil)
			assert.Equal(t, 401, status)
			assert.Equal(t, "UNAUTHENTICATED", errCode(t, body))
		})
	}
}

func TestAdminCreateVenueValidation(t *testing.T) {
	srv := newTestServer(t)
	admin := registerAndLogin(t, srv, testAdminEmail)
	b := venueBody()
	b["sections"] = []map[string]any{}
	status, body := call(t, srv, "POST", "/v1/admin/venues", admin.AccessToken, b)
	assert.Equal(t, 400, status)
	assert.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}

func TestAdminInvariants(t *testing.T) {
	srv := newTestServer(t)
	admin := registerAndLogin(t, srv, testAdminEmail)
	status, body := call(t, srv, "GET", "/v1/admin/invariants", admin.AccessToken, nil)
	require.Equal(t, 200, status, string(body))
	assert.Contains(t, string(body), `"total":0`)

	user := registerAndLogin(t, srv, "user@example.com")
	status, _ = call(t, srv, "GET", "/v1/admin/invariants", user.AccessToken, nil)
	assert.Equal(t, 403, status)
}
