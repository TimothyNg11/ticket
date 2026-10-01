package auth

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTicketSignRoundTrip(t *testing.T) {
	s := NewTicketSigner(secret)
	tid, sid := uuid.New(), uuid.New()
	gotT, gotS, err := s.Verify(s.Sign(tid, sid))
	require.NoError(t, err)
	assert.Equal(t, tid, gotT)
	assert.Equal(t, sid, gotS)
}

func TestTicketVerifyRejectsTamperedPayload(t *testing.T) {
	s := NewTicketSigner(secret)
	tok := s.Sign(uuid.New(), uuid.New())
	payload, sig, _ := strings.Cut(tok, ".")
	other := s.Sign(uuid.New(), uuid.New())
	otherPayload, _, _ := strings.Cut(other, ".")
	_, _, err := s.Verify(otherPayload + "." + sig)
	assert.ErrorIs(t, err, ErrInvalidToken)
	_, _, err = s.Verify(payload + "." + sig + "x")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestTicketVerifyRejectsOtherKey(t *testing.T) {
	tok := NewTicketSigner([]byte("another-secret-another-secret-xx")).Sign(uuid.New(), uuid.New())
	_, _, err := NewTicketSigner(secret).Verify(tok)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestTicketVerifyRejectsGarbage(t *testing.T) {
	s := NewTicketSigner(secret)
	for _, tok := range []string{"", "abc", "a.b", "!!!.!!!"} {
		_, _, err := s.Verify(tok)
		assert.ErrorIs(t, err, ErrInvalidToken, tok)
	}
}
