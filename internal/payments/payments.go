// Package payments talks to the payment provider. In this project the provider is
// a mock (see package mock and services/payments_mock), but the client treats it
// like a real one: every call carries an idempotency key, and any answer that isn't
// a definite yes or no is reported as ErrUnknownOutcome.
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Status is a definite provider answer.
type Status string

const (
	Succeeded Status = "succeeded"
	Declined  Status = "declined"
)

// Result is the provider's answer for a charge or refund.
type Result struct {
	Status Status `json:"status"`
	Ref    string `json:"id"`
}

var (
	// ErrUnknownOutcome means the call timed out or the provider failed, so the
	// charge may or may not have happened. Never treat it as a decline: ask the
	// provider again later (GetCharge) using the same idempotency key.
	ErrUnknownOutcome = errors.New("payments: outcome unknown")
	// ErrNotFound means the provider has no charge for that idempotency key.
	ErrNotFound = errors.New("payments: not found")
)

// Client is the provider API the booking code depends on.
type Client interface {
	Charge(ctx context.Context, idempotencyKey string, amountCents int) (Result, error)
	Refund(ctx context.Context, idempotencyKey, chargeRef string, amountCents int) (Result, error)
	GetCharge(ctx context.Context, idempotencyKey string) (Result, error)
}

// HTTPClient implements Client over the provider's JSON API.
type HTTPClient struct {
	base string
	hc   *http.Client
}

// NewHTTPClient returns a client whose calls give up after timeout.
func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{base: baseURL, hc: &http.Client{Timeout: timeout}}
}

// Charge asks the provider to charge amountCents. Repeating a key returns the
// original result instead of charging twice.
func (c *HTTPClient) Charge(ctx context.Context, key string, amountCents int) (Result, error) {
	return c.post(ctx, "/charges", map[string]any{"idempotency_key": key, "amount_cents": amountCents})
}

// Refund refunds a previous charge.
func (c *HTTPClient) Refund(ctx context.Context, key, chargeRef string, amountCents int) (Result, error) {
	return c.post(ctx, "/refunds", map[string]any{"idempotency_key": key, "charge_id": chargeRef, "amount_cents": amountCents})
}

// GetCharge looks up a charge by the idempotency key it was created with.
func (c *HTTPClient) GetCharge(ctx context.Context, key string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/charges/"+url.PathEscape(key), nil)
	if err != nil {
		return Result{}, err
	}
	return c.do(req)
}

func (c *HTTPClient) post(ctx context.Context, path string, body any) (Result, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *HTTPClient) do(req *http.Request) (Result, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		// Timeouts and connection errors: the request may have reached the provider.
		return Result{}, fmt.Errorf("%w: %v", ErrUnknownOutcome, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Result{}, ErrNotFound
	case resp.StatusCode >= 500:
		return Result{}, fmt.Errorf("%w: provider status %d", ErrUnknownOutcome, resp.StatusCode)
	case resp.StatusCode >= 300:
		return Result{}, fmt.Errorf("payments: provider rejected request: status %d", resp.StatusCode)
	}
	var r Result
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Result{}, fmt.Errorf("%w: bad response: %v", ErrUnknownOutcome, err)
	}
	if r.Status != Succeeded && r.Status != Declined {
		return Result{}, fmt.Errorf("%w: unexpected status %q", ErrUnknownOutcome, r.Status)
	}
	return r, nil
}
