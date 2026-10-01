package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRefreshToken(t *testing.T) {
	tok, hash, err := NewRefreshToken()
	require.NoError(t, err)
	assert.Len(t, tok, 43, "32 random bytes, base64url without padding")
	assert.Equal(t, HashRefreshToken(tok), hash)
	assert.NotEqual(t, tok, hash, "only the hash is stored")

	tok2, _, _ := NewRefreshToken()
	assert.NotEqual(t, tok, tok2)
}
