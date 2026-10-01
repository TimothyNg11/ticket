// Package account implements registration, login, and refresh-token rotation.
package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/apperr"
	"ticket/internal/auth"
	"ticket/internal/db"
	"ticket/internal/db/sqlc"
)

var (
	errEmailTaken     = apperr.Conflict("EMAIL_TAKEN", "email already registered")
	errBadCredentials = &apperr.Error{Status: http.StatusUnauthorized, Code: "INVALID_CREDENTIALS", Message: "invalid email or password"}
	errInvalidRefresh = &apperr.Error{Status: http.StatusUnauthorized, Code: "INVALID_REFRESH_TOKEN", Message: "invalid or expired refresh token"}
	errRefreshReused  = &apperr.Error{Status: http.StatusUnauthorized, Code: "REFRESH_TOKEN_REUSED", Message: "refresh token reuse detected; please log in again"}
)

// dummyHash is verified against when an email is unknown, so login takes the same
// time whether or not the account exists (prevents user enumeration by timing).
var dummyHash, _ = auth.HashPassword("dummy-password-for-timing")

// Tokens is what a successful login or refresh returns to the client.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresIn time.Duration
}

// Service implements account operations against Postgres.
type Service struct {
	pool        *pgxpool.Pool
	tokens      *auth.TokenIssuer
	refreshTTL  time.Duration
	adminEmails map[string]bool
}

// New returns a Service. Emails in adminEmails (lower-cased) become admins on registration.
func New(pool *pgxpool.Pool, tokens *auth.TokenIssuer, refreshTTL time.Duration, adminEmails map[string]bool) *Service {
	return &Service{pool: pool, tokens: tokens, refreshTTL: refreshTTL, adminEmails: adminEmails}
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Register creates a user. Duplicate emails (ignoring case) return EMAIL_TAKEN;
// the unique constraint, not a prior SELECT, decides races between concurrent sign-ups.
func (s *Service) Register(ctx context.Context, email, password string) (sqlc.User, error) {
	email = normalizeEmail(email)
	hash, err := auth.HashPassword(password)
	if err != nil {
		return sqlc.User{}, err
	}
	role := "user"
	if s.adminEmails[email] {
		role = "admin"
	}
	u, err := sqlc.New(s.pool).CreateUser(ctx, sqlc.CreateUserParams{Email: email, PasswordHash: hash, Role: role})
	if db.IsUniqueViolation(err) {
		return sqlc.User{}, errEmailTaken
	}
	return u, err
}

// Login checks credentials and starts a new refresh-token family.
func (s *Service) Login(ctx context.Context, email, password string) (Tokens, error) {
	q := sqlc.New(s.pool)
	u, err := q.GetUserByEmail(ctx, normalizeEmail(email))
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = auth.VerifyPassword(password, dummyHash)
		return Tokens{}, errBadCredentials
	}
	if err != nil {
		return Tokens{}, err
	}
	ok, err := auth.VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return Tokens{}, err
	}
	if !ok {
		return Tokens{}, errBadCredentials
	}
	t, _, err := s.issue(ctx, q, u, uuid.New())
	return t, err
}

// issue creates an access token and a new refresh token in the given family,
// returning the tokens and the new refresh token's row id.
func (s *Service) issue(ctx context.Context, q *sqlc.Queries, u sqlc.User, family uuid.UUID) (Tokens, uuid.UUID, error) {
	refresh, hash, err := auth.NewRefreshToken()
	if err != nil {
		return Tokens{}, uuid.Nil, err
	}
	rt, err := q.CreateRefreshToken(ctx, sqlc.CreateRefreshTokenParams{
		UserID: u.ID, FamilyID: family, TokenHash: hash, ExpiresAt: time.Now().Add(s.refreshTTL),
	})
	if err != nil {
		return Tokens{}, uuid.Nil, err
	}
	access, err := s.tokens.Issue(u.ID, u.Role)
	if err != nil {
		return Tokens{}, uuid.Nil, err
	}
	return Tokens{Access: access, Refresh: refresh, ExpiresIn: s.tokens.TTL()}, rt.ID, nil
}

// Refresh exchanges a refresh token for a new pair, revoking the old one. If the
// presented token was already revoked, someone is replaying a stolen copy: the whole
// family is revoked so neither the thief nor the victim can keep using it.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	var out Tokens
	reused := false
	err := db.InTx(ctx, s.pool, func(q *sqlc.Queries) error {
		rt, err := q.GetRefreshTokenForUpdate(ctx, auth.HashRefreshToken(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvalidRefresh
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil {
			reused = true
			// Return nil so the family revocation commits; the error is reported below.
			return q.RevokeRefreshTokenFamily(ctx, rt.FamilyID)
		}
		if time.Now().After(rt.ExpiresAt) {
			return errInvalidRefresh
		}
		u, err := q.GetUserByID(ctx, rt.UserID)
		if err != nil {
			return err
		}
		var nextID uuid.UUID
		if out, nextID, err = s.issue(ctx, q, u, rt.FamilyID); err != nil {
			return err
		}
		return q.RevokeRefreshToken(ctx, sqlc.RevokeRefreshTokenParams{ID: rt.ID, ReplacedBy: &nextID})
	})
	if err != nil {
		return Tokens{}, err
	}
	if reused {
		return Tokens{}, errRefreshReused
	}
	return out, nil
}

// Logout revokes the refresh token. It succeeds even if the token is unknown or
// already revoked, so clients can retry it safely.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	return sqlc.New(s.pool).RevokeRefreshTokenByHash(ctx, auth.HashRefreshToken(refreshToken))
}
