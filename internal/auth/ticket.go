package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/google/uuid"
)

// TicketSigner produces the QR payload printed on a ticket: the ticket and seat
// ids plus an HMAC over them. Anyone can read a QR code, but without the key nobody
// can forge one, so the gate scanner can trust the ids before checking the database
// (where a cancelled ticket shows up as void).
type TicketSigner struct {
	key []byte
}

// NewTicketSigner returns a signer using key (keep it separate from the JWT secret).
func NewTicketSigner(key []byte) *TicketSigner { return &TicketSigner{key: key} }

var b64url = base64.RawURLEncoding

// Sign returns "<base64url(ticketID|eventSeatID)>.<base64url(hmac)>".
func (s *TicketSigner) Sign(ticketID, eventSeatID uuid.UUID) string {
	payload := b64url.EncodeToString([]byte(ticketID.String() + "|" + eventSeatID.String()))
	return payload + "." + b64url.EncodeToString(s.mac(payload))
}

// Verify checks the signature and returns the ids it covers.
func (s *TicketSigner) Verify(token string) (ticketID, eventSeatID uuid.UUID, err error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	got, err := b64url.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	raw, err := b64url.DecodeString(payload)
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	tPart, sPart, ok := strings.Cut(string(raw), "|")
	if !ok {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	if ticketID, err = uuid.Parse(tPart); err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	if eventSeatID, err = uuid.Parse(sPart); err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	return ticketID, eventSeatID, nil
}

func (s *TicketSigner) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
