package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken covers every reason a token is rejected (bad signature, expired,
// malformed). Callers should not tell clients which one it was.
var ErrInvalidToken = errors.New("auth: invalid token")

// Claims is what the API trusts about a caller after verifying their access token.
type Claims struct {
	UserID uuid.UUID
	Role   string
	// JTI uniquely identifies the token so it can be revoked before it expires.
	JTI       string
	ExpiresAt time.Time
}

type jwtClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// TokenIssuer signs and verifies short-lived HS256 access tokens.
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

// NewTokenIssuer returns an issuer whose tokens expire after ttl.
func NewTokenIssuer(secret []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, ttl: ttl}
}

// TTL is how long issued tokens stay valid.
func (ti *TokenIssuer) TTL() time.Duration { return ti.ttl }

// Issue returns a signed access token for the user.
func (ti *TokenIssuer) Issue(userID uuid.UUID, role string) (string, error) {
	now := time.Now()
	c := jwtClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ti.ttl)),
			ID:        uuid.NewString(), // jti: lets individual tokens be revoked later
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(ti.secret)
}

// Verify checks the signature and expiry and returns the token's claims.
func (ti *TokenIssuer) Verify(token string) (Claims, error) {
	var c jwtClaims
	// Pinning the algorithm blocks "alg: none" and algorithm-confusion attacks.
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return ti.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired())
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	return Claims{UserID: id, Role: c.Role, JTI: c.ID, ExpiresAt: c.ExpiresAt.Time}, nil
}
