package payments_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ticket/internal/payments"
	"ticket/internal/payments/mock"
)

// seq returns a rnd func that yields vals in order, then 0.99 forever.
func seq(vals ...float64) func() float64 {
	return func() float64 {
		if len(vals) == 0 {
			return 0.99
		}
		v := vals[0]
		vals = vals[1:]
		return v
	}
}

func setup(t *testing.T, cfg mock.Config, rnd func() float64) (*mock.Server, *payments.HTTPClient) {
	m := mock.New(cfg, rnd)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, payments.NewHTTPClient(srv.URL, 500*time.Millisecond)
}

func TestMockChargeIsIdempotentByKey(t *testing.T) {
	m, c := setup(t, mock.Config{}, nil)
	ctx := context.Background()
	a, err := c.Charge(ctx, "k1", 100)
	require.NoError(t, err)
	b, err := c.Charge(ctx, "k1", 100)
	require.NoError(t, err)
	assert.Equal(t, payments.Succeeded, a.Status)
	assert.Equal(t, a, b)
	assert.Equal(t, 1, m.Charges())
}

func TestMockDecline(t *testing.T) {
	// rnd: error roll (no), record-first roll, decline roll (yes)
	_, c := setup(t, mock.Config{DeclineRate: 0.5}, seq(0.9, 0.9, 0.1))
	r, err := c.Charge(context.Background(), "k", 100)
	require.NoError(t, err)
	assert.Equal(t, payments.Declined, r.Status)
}

func TestErrorIsUnknownOutcomeAndMayHaveCharged(t *testing.T) {
	// error roll (yes), record-first roll (yes), decline roll (no)
	m, c := setup(t, mock.Config{ErrorRate: 0.5}, seq(0.1, 0.1, 0.9))
	ctx := context.Background()
	_, err := c.Charge(ctx, "k", 100)
	assert.ErrorIs(t, err, payments.ErrUnknownOutcome)
	assert.Equal(t, 1, m.Charges(), "the charge went through even though the response failed")

	r, err := c.GetCharge(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, payments.Succeeded, r.Status)
}

func TestTimeoutIsUnknownOutcome(t *testing.T) {
	_, c := setup(t, mock.Config{LatencyMS: 1000}, nil)
	_, err := c.Charge(context.Background(), "k", 100)
	assert.ErrorIs(t, err, payments.ErrUnknownOutcome)
}

func TestGetChargeNotFound(t *testing.T) {
	_, c := setup(t, mock.Config{}, nil)
	_, err := c.GetCharge(context.Background(), "never")
	assert.ErrorIs(t, err, payments.ErrNotFound)
}

func TestRefund(t *testing.T) {
	_, c := setup(t, mock.Config{}, nil)
	ctx := context.Background()
	ch, err := c.Charge(ctx, "k", 100)
	require.NoError(t, err)
	r, err := c.Refund(ctx, "refund-k", ch.Ref, 100)
	require.NoError(t, err)
	assert.Equal(t, payments.Succeeded, r.Status)
}

func TestMockAdminConfig(t *testing.T) {
	m := mock.New(mock.Config{}, nil)
	srv := httptest.NewServer(m)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/admin/config", bytes.NewBufferString(`{"decline_rate":1}`))
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	r, err := payments.NewHTTPClient(srv.URL, time.Second).Charge(context.Background(), "k", 100)
	require.NoError(t, err)
	assert.Equal(t, payments.Declined, r.Status)
}
