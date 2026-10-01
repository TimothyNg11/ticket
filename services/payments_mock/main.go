// Command payments_mock serves the fake payment provider from package mock.
//
// Behavior is set by DECLINE_RATE, ERROR_RATE (0..1) and LATENCY_MS, and can be
// changed while running with PUT /admin/config, which chaos and load tests use
// to inject slowness and failures mid-sale.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"ticket/internal/payments/mock"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := mock.Config{
		DeclineRate: envFloat("DECLINE_RATE"),
		ErrorRate:   envFloat("ERROR_RATE"),
		LatencyMS:   int(envFloat("LATENCY_MS")),
	}
	addr := ":" + envOr("PORT", "8081")
	log.Info("payments mock listening", "addr", addr, "config", cfg)
	srv := &http.Server{Addr: addr, Handler: mock.New(cfg, nil), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func envFloat(k string) float64 {
	f, _ := strconv.ParseFloat(os.Getenv(k), 64)
	return f
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
