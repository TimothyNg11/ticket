// Package mock is a fake payment provider with dials for latency, declines, and
// failures, so tests and load runs can exercise every payment outcome on demand.
//
// It behaves like a careful real provider: results are stored by idempotency key,
// so repeating a request never charges twice. Injected failures happen *after*
// the charge is recorded half of the time, which reproduces the nasty real-world
// case where the money moved but the response was lost.
package mock

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Config controls injected behavior. It can be changed at runtime via PUT /admin/config.
type Config struct {
	DeclineRate float64 `json:"decline_rate"` // probability a new charge is declined
	ErrorRate   float64 `json:"error_rate"`   // probability a request returns 500
	LatencyMS   int     `json:"latency_ms"`   // delay before every response
}

type record struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AmountCents int    `json:"amount_cents"`
}

// Server is the provider. The zero value is not usable; call New.
type Server struct {
	mu      sync.Mutex
	cfg     Config
	rnd     func() float64
	charges map[string]record // by idempotency key
	refunds map[string]record
	calls   int // charge requests that created a new charge
	mux     *http.ServeMux
}

// New returns a provider. rnd supplies randomness in [0,1); nil uses math/rand.
func New(cfg Config, rnd func() float64) *Server {
	if rnd == nil {
		rnd = rand.Float64
	}
	s := &Server{cfg: cfg, rnd: rnd, charges: map[string]record{}, refunds: map[string]record{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /charges", s.charge)
	s.mux.HandleFunc("GET /charges/{key}", s.getCharge)
	s.mux.HandleFunc("POST /refunds", s.refund)
	s.mux.HandleFunc("PUT /admin/config", s.setConfig)
	s.mux.HandleFunc("GET /admin/stats", s.stats)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// SetConfig replaces the injected behavior.
func (s *Server) SetConfig(c Config) {
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
}

// Charges reports how many distinct charges were created.
func (s *Server) Charges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *Server) charge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key    string `json:"idempotency_key"`
		Amount int    `json:"amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" || req.Amount <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	rec, seen := s.charges[req.Key]
	failNow := s.rnd() < cfg.ErrorRate
	recordFirst := s.rnd() < 0.5
	if !seen && (!failNow || recordFirst) {
		status := "succeeded"
		if s.rnd() < cfg.DeclineRate {
			status = "declined"
		}
		rec = record{ID: "ch_" + uuid.NewString(), Status: status, AmountCents: req.Amount}
		s.charges[req.Key] = rec
		s.calls++
	}
	s.mu.Unlock()

	time.Sleep(time.Duration(cfg.LatencyMS) * time.Millisecond)
	if failNow {
		http.Error(w, "provider error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rec)
}

func (s *Server) getCharge(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rec, ok := s.charges[r.PathValue("key")]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, rec)
}

func (s *Server) refund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key      string `json:"idempotency_key"`
		ChargeID string `json:"charge_id"`
		Amount   int    `json:"amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	failNow := s.rnd() < cfg.ErrorRate
	rec, seen := s.refunds[req.Key]
	if !seen && !failNow {
		rec = record{ID: "re_" + uuid.NewString(), Status: "succeeded", AmountCents: req.Amount}
		s.refunds[req.Key] = rec
	}
	s.mu.Unlock()
	time.Sleep(time.Duration(cfg.LatencyMS) * time.Millisecond)
	if failNow {
		http.Error(w, "provider error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rec)
}

func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	var c Config
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.SetConfig(c)
	writeJSON(w, c)
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{"charges": s.calls, "refunds": len(s.refunds), "config": s.cfg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
