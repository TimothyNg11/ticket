package auth

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Token types. Every token the API signs carries one, and every verifier checks
// it, so a token minted for one purpose (say, an admission pass) can never be
// replayed as another (an access token), even though they share a signing key.
const (
	TypeAccess    = "access"
	TypeQueue     = "queue"
	TypeAdmission = "admission"
)

// EventPass is a typed token binding a user to an event: the waiting-room
// position token and the admission token are both EventPasses.
type EventPass struct {
	UserID    uuid.UUID
	EventID   uuid.UUID
	ExpiresAt time.Time
}

type passClaims struct {
	Type  string `json:"typ"`
	Event string `json:"ev"`
	jwt.RegisteredClaims
}

// PassIssuer signs and verifies EventPasses.
type PassIssuer struct{ secret []byte }

// NewPassIssuer returns an issuer using secret.
func NewPassIssuer(secret []byte) *PassIssuer { return &PassIssuer{secret: secret} }

// Issue signs a pass of the given type, valid for ttl.
func (p *PassIssuer) Issue(typ string, userID, eventID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	exp := time.Now().Add(ttl)
	c := passClaims{Type: typ, Event: eventID.String(), RegisteredClaims: jwt.RegisteredClaims{
		Subject: userID.String(), ExpiresAt: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(time.Now()),
	}}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(p.secret)
	return s, exp, err
}

// Verify checks signature, expiry, and type, and returns the pass.
func (p *PassIssuer) Verify(typ, token string) (EventPass, error) {
	var c passClaims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return p.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || c.Type != typ {
		return EventPass{}, ErrInvalidToken
	}
	user, err1 := uuid.Parse(c.Subject)
	event, err2 := uuid.Parse(c.Event)
	if err1 != nil || err2 != nil {
		return EventPass{}, ErrInvalidToken
	}
	return EventPass{UserID: user, EventID: event, ExpiresAt: c.ExpiresAt.Time}, nil
}
