package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pw = "a-long-enough-password"

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// registerAndLogin creates an account and returns its tokens.
func registerAndLogin(t *testing.T, srv *httptest.Server, email string) tokenResp {
	t.Helper()
	status, body := call(t, srv, "POST", "/v1/auth/register", "", map[string]string{"email": email, "password": pw})
	require.Equal(t, 201, status, string(body))
	status, body = call(t, srv, "POST", "/v1/auth/login", "", map[string]string{"email": email, "password": pw})
	require.Equal(t, 200, status, string(body))
	var tp tokenResp
	require.NoError(t, json.Unmarshal(body, &tp))
	return tp
}

func TestRegisterLoginRefreshLogout(t *testing.T) {
	srv := newTestServer(t)
	tp := registerAndLogin(t, srv, "alice@example.com")
	assert.Equal(t, "Bearer", tp.TokenType)
	assert.Equal(t, 900, tp.ExpiresIn)

	status, body := call(t, srv, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": tp.RefreshToken})
	require.Equal(t, 200, status, string(body))
	var next tokenResp
	require.NoError(t, json.Unmarshal(body, &next))

	status, body = call(t, srv, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": tp.RefreshToken})
	assert.Equal(t, 401, status)
	assert.Equal(t, "REFRESH_TOKEN_REUSED", errCode(t, body))

	status, _ = call(t, srv, "POST", "/v1/auth/logout", "", map[string]string{"refresh_token": next.RefreshToken})
	assert.Equal(t, 204, status)
}

func TestRegisterValidation(t *testing.T) {
	srv := newTestServer(t)
	for name, body := range map[string]map[string]string{
		"bad email":      {"email": "not-an-email", "password": pw},
		"short password": {"email": "a@example.com", "password": "short"},
		"missing":        {},
	} {
		t.Run(name, func(t *testing.T) {
			status, resp := call(t, srv, "POST", "/v1/auth/register", "", body)
			assert.Equal(t, 400, status)
			assert.Equal(t, "VALIDATION_FAILED", errCode(t, resp))
		})
	}
}

func TestRegisterDuplicateIgnoringCaseIs409(t *testing.T) {
	srv := newTestServer(t)
	registerAndLogin(t, srv, "alice@example.com")
	status, body := call(t, srv, "POST", "/v1/auth/register", "", map[string]string{"email": "Alice@Example.com", "password": pw})
	assert.Equal(t, 409, status)
	assert.Equal(t, "EMAIL_TAKEN", errCode(t, body))
}

func TestConcurrentRegisterNever500(t *testing.T) {
	srv := newTestServer(t)
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _ = call(t, srv, "POST", "/v1/auth/register", "", map[string]string{"email": "race@example.com", "password": pw})
		}()
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		require.Contains(t, []int{201, 409}, s)
		if s == 201 {
			created++
		}
	}
	assert.Equal(t, 1, created)
}

func TestLoginWrongPasswordIs401(t *testing.T) {
	srv := newTestServer(t)
	registerAndLogin(t, srv, "alice@example.com")
	status, body := call(t, srv, "POST", "/v1/auth/login", "", map[string]string{"email": "alice@example.com", "password": "wrong-password-xx"})
	assert.Equal(t, 401, status)
	assert.Equal(t, "INVALID_CREDENTIALS", errCode(t, body))
}
