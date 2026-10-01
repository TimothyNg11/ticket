package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func TestIssueAndVerify(t *testing.T) {
	ti := NewTokenIssuer(secret, time.Minute)
	id := uuid.New()
	tok, err := ti.Issue(id, "admin")
	require.NoError(t, err)

	c, err := ti.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, Claims{UserID: id, Role: "admin"}, c)
}

func TestVerifyRejectsExpired(t *testing.T) {
	tok, _ := NewTokenIssuer(secret, -time.Minute).Issue(uuid.New(), "user")
	_, err := NewTokenIssuer(secret, time.Minute).Verify(tok)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok, _ := NewTokenIssuer([]byte("another-secret-another-secret-xx"), time.Minute).Issue(uuid.New(), "user")
	_, err := NewTokenIssuer(secret, time.Minute).Verify(tok)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	// The classic JWT attack: an unsigned token claiming to be admin.
	claims := jwt.MapClaims{"sub": uuid.NewString(), "role": "admin", "exp": time.Now().Add(time.Hour).Unix()}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	_, err = NewTokenIssuer(secret, time.Minute).Verify(tok)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsGarbage(t *testing.T) {
	_, err := NewTokenIssuer(secret, time.Minute).Verify("not.a.jwt")
	assert.ErrorIs(t, err, ErrInvalidToken)
}
