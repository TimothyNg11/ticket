package account_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/account"
	"ticket/internal/apperr"
	"ticket/internal/auth"
	"ticket/internal/testutil"
)

var pg *testutil.PG

func TestMain(m *testing.M) { os.Exit(testutil.RunWithPostgres(m, &pg)) }

const pw = "a-long-enough-password"

func newService(t *testing.T) *account.Service {
	pool := pg.NewDB(t)
	tokens := auth.NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	return account.New(pool, tokens, time.Hour, map[string]bool{"boss@example.com": true})
}

func code(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestRegisterNormalizesEmailAndAssignsRole(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	u, err := s.Register(ctx, "  Alice@Example.COM ", pw)
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", u.Email)
	assert.Equal(t, "user", u.Role)

	boss, err := s.Register(ctx, "Boss@example.com", pw)
	require.NoError(t, err)
	assert.Equal(t, "admin", boss.Role)
}

func TestRegisterDuplicateEmailIgnoringCase(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	_, err := s.Register(ctx, "alice@example.com", pw)
	require.NoError(t, err)
	_, err = s.Register(ctx, "ALICE@example.com", pw)
	assert.Equal(t, "EMAIL_TAKEN", code(err))
}

func TestConcurrentRegisterSameEmail(t *testing.T) {
	s := newService(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Register(context.Background(), "race@example.com", pw)
		}()
	}
	wg.Wait()
	ok, taken := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case code(err) == "EMAIL_TAKEN":
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 7, taken)
}

func TestLogin(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	_, err := s.Register(ctx, "alice@example.com", pw)
	require.NoError(t, err)

	toks, err := s.Login(ctx, "Alice@Example.com", pw)
	require.NoError(t, err)
	assert.NotEmpty(t, toks.Access)
	assert.NotEmpty(t, toks.Refresh)
	assert.Equal(t, time.Minute, toks.ExpiresIn)

	_, err = s.Login(ctx, "alice@example.com", "wrong-password-123")
	assert.Equal(t, "INVALID_CREDENTIALS", code(err))
	_, err = s.Login(ctx, "nobody@example.com", pw)
	assert.Equal(t, "INVALID_CREDENTIALS", code(err), "unknown email gets the same error as a wrong password")
}

func TestRefreshRotatesAndDetectsReuse(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	_, err := s.Register(ctx, "alice@example.com", pw)
	require.NoError(t, err)
	first, err := s.Login(ctx, "alice@example.com", pw)
	require.NoError(t, err)

	second, err := s.Refresh(ctx, first.Refresh)
	require.NoError(t, err)
	assert.NotEqual(t, first.Refresh, second.Refresh)

	// Replaying the rotated token is treated as theft...
	_, err = s.Refresh(ctx, first.Refresh)
	assert.Equal(t, "REFRESH_TOKEN_REUSED", code(err))
	// ...and kills the legitimate successor too.
	_, err = s.Refresh(ctx, second.Refresh)
	assert.Equal(t, "REFRESH_TOKEN_REUSED", code(err))
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	s := newService(t)
	_, err := s.Refresh(context.Background(), "made-up")
	assert.Equal(t, "INVALID_REFRESH_TOKEN", code(err))
}

func TestLogoutRevokes(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	_, err := s.Register(ctx, "alice@example.com", pw)
	require.NoError(t, err)
	toks, err := s.Login(ctx, "alice@example.com", pw)
	require.NoError(t, err)

	require.NoError(t, s.Logout(ctx, toks.Refresh))
	require.NoError(t, s.Logout(ctx, toks.Refresh), "logout is idempotent")
	_, err = s.Refresh(ctx, toks.Refresh)
	assert.Error(t, err)
}
