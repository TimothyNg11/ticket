package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/db",
		"JWT_SECRET":   "0123456789abcdef0123456789abcdef",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(valid()))
	require.NoError(t, err)
	assert.Equal(t, ":8080", c.HTTPAddr)
	assert.Equal(t, 15*time.Minute, c.AccessTokenTTL)
	assert.Equal(t, 30*24*time.Hour, c.RefreshTokenTTL)
	assert.Empty(t, c.AdminEmails)
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	m := valid()
	delete(m, "DATABASE_URL")
	_, err := Load(env(m))
	assert.ErrorContains(t, err, "DATABASE_URL")
}

func TestLoadRejectsShortSecret(t *testing.T) {
	m := valid()
	m["JWT_SECRET"] = "short"
	_, err := Load(env(m))
	assert.ErrorContains(t, err, "JWT_SECRET")
}

func TestLoadParsesOverrides(t *testing.T) {
	m := valid()
	m["HTTP_ADDR"] = ":9000"
	m["ACCESS_TOKEN_TTL"] = "5m"
	m["ADMIN_EMAILS"] = " Admin@Example.com , ops@example.com,"
	c, err := Load(env(m))
	require.NoError(t, err)
	assert.Equal(t, ":9000", c.HTTPAddr)
	assert.Equal(t, 5*time.Minute, c.AccessTokenTTL)
	assert.Equal(t, map[string]bool{"admin@example.com": true, "ops@example.com": true}, c.AdminEmails)
}

func TestLoadRejectsBadDuration(t *testing.T) {
	m := valid()
	m["REFRESH_TOKEN_TTL"] = "forever"
	_, err := Load(env(m))
	assert.ErrorContains(t, err, "REFRESH_TOKEN_TTL")
}
