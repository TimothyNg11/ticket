package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPassRoundTrip(t *testing.T) {
	p := NewPassIssuer(secret)
	u, e := uuid.New(), uuid.New()
	tok, exp, err := p.Issue(TypeAdmission, u, e, time.Minute)
	require.NoError(t, err)
	got, err := p.Verify(TypeAdmission, tok)
	require.NoError(t, err)
	assert.Equal(t, u, got.UserID)
	assert.Equal(t, e, got.EventID)
	assert.WithinDuration(t, exp, got.ExpiresAt, time.Second)
}

func TestPassTypeIsEnforced(t *testing.T) {
	p := NewPassIssuer(secret)
	queueTok, _, err := p.Issue(TypeQueue, uuid.New(), uuid.New(), time.Minute)
	require.NoError(t, err)
	_, err = p.Verify(TypeAdmission, queueTok)
	assert.ErrorIs(t, err, ErrInvalidToken, "a queue token is not an admission pass")

	// The same secret signs access tokens; neither kind may stand in for the other.
	_, err = NewTokenIssuer(secret, time.Minute).Verify(queueTok)
	assert.ErrorIs(t, err, ErrInvalidToken, "a pass is not an access token")
	access, _ := NewTokenIssuer(secret, time.Minute).Issue(uuid.New(), "admin")
	_, err = p.Verify(TypeAdmission, access)
	assert.ErrorIs(t, err, ErrInvalidToken, "an access token is not a pass")
}

func TestPassExpires(t *testing.T) {
	p := NewPassIssuer(secret)
	tok, _, _ := p.Issue(TypeAdmission, uuid.New(), uuid.New(), -time.Second)
	_, err := p.Verify(TypeAdmission, tok)
	assert.ErrorIs(t, err, ErrInvalidToken)
}
