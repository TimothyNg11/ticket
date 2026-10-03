// Command api serves the ticketing HTTP API.
//
// Subcommands:
//
//	api              serve HTTP
//	api migrate      apply database migrations and exit (Compose/Kubernetes job)
//	api healthcheck  GET /healthz on localhost; used by the container HEALTHCHECK,
//	                 since the distroless image has no curl
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"ticket/internal/account"
	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/cache"
	"ticket/internal/config"
	"ticket/internal/db"
	"ticket/internal/httpapi"
	"ticket/internal/inventory"
	"ticket/internal/payments"
	"ticket/internal/ratelimit"
	"ticket/internal/waitingroom"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "":
		err = serve(log)
	case "migrate":
		err = db.Migrate(os.Getenv("DATABASE_URL"))
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("fatal", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	auth.SetHashConcurrency(cfg.HashConcurrency)
	ropt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(ropt)
	defer func() { _ = rdb.Close() }()

	inv := inventory.New(pool)
	c := cache.New(rdb, inv, log)
	// Retries are safe because every charge carries the order's idempotency key;
	// the breaker makes checkout fail fast while the provider is down.
	pay := payments.NewResilient(payments.NewHTTPClient(cfg.PaymentsURL, 3*time.Second),
		payments.DefaultRetry, payments.NewBreaker(5, 10*time.Second), nil)
	book := booking.New(pool, pay, auth.NewTicketSigner(cfg.TicketSigningKey), cfg.HoldTTL)
	book.OnSeatsChanged(c.SeatsChanged)
	limiter := ratelimit.New(rdb, cfg.RateLimitScale, func(err error) {
		log.Warn("rate limiter falling back to in-process limits", "err", err)
	})

	tokens := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL)
	var draining atomic.Bool
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Deps{
			Draining:   &draining,
			Pool:       pool,
			Tokens:     tokens,
			Accounts:   account.New(pool, tokens, cfg.RefreshTokenTTL, cfg.AdminEmails),
			Inventory:  inv,
			Booking:    book,
			Cache:      c,
			Room:       waitingroom.New(rdb, auth.NewPassIssuer(cfg.JWTSecret)),
			Limiter:    limiter,
			TrustProxy: cfg.TrustProxy,
			Log:        log,
		}),
		// Bound every phase of a connection so slow or idle clients can't pin goroutines.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.HTTPAddr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Graceful shutdown, in the order Kubernetes needs:
	//  1. Fail readiness so the load balancer stops sending new requests. Endpoint
	//     removal takes a few seconds to propagate, so keep serving meanwhile.
	//  2. Stop accepting connections and let in-flight requests finish.
	//  3. Close the database and Redis pools (deferred above).
	draining.Store(true)
	log.Info("draining", "delay", cfg.DrainDelay.String())
	time.Sleep(cfg.DrainDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("shut down cleanly")
	return nil
}

func healthcheck() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	client := http.Client{Timeout: 2 * time.Second}
	// addr comes from our own HTTP_ADDR setting, not from a request.
	resp, err := client.Get("http://localhost" + addr + "/healthz") //nolint:gosec,noctx // see above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
