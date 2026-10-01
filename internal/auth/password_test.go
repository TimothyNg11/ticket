package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$"))

	ok, err := VerifyPassword("correct horse battery staple", h)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = VerifyPassword("wrong", h)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	assert.NotEqual(t, a, b)
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	_, err := VerifyPassword("x", "$bcrypt$nope")
	assert.Error(t, err)
}
