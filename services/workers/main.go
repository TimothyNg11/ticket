// Command workers runs background jobs. Each job is a subcommand so it can be
// deployed and scaled on its own:
//
//	workers sweeper        release expired holds every SWEEP_INTERVAL (default 15s)
//	workers availability   recount seats per on-sale event into Redis every minute,
//	                       correcting drift in the cached availability counters
//	workers admitter       admit the next batch from each event's waiting room,
//	                       once per the event's admission interval
//	workers reconciler     settle orders stuck in pending_payment and retry failed
//	                       refunds, every RECONCILE_INTERVAL (default 15s)
//	workers outbox-relay   publish outbox rows to the "events" Redis Stream
//	workers notifier       consume order events and send (log) notifications
//	workers invariants     check the core guarantees every 30s and export the
//	                       violation counts (alerts fire on anything above zero)
//
// Every job serves Prometheus metrics on METRICS_ADDR (default :9090).
//
// Every job is safe to run as several replicas at once: work is claimed with
// FOR UPDATE SKIP LOCKED, so replicas never process the same row.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/cache"
	"ticket/internal/idempotency"
	"ticket/internal/inventory"
	"ticket/internal/metrics"
	"ticket/internal/observability"
	"ticket/internal/outbox"
	"ticket/internal/payments"
	"ticket/internal/waitingroom"
)

func main() {
	log := observability.NewLogger("workers")
	if len(os.Args) < 2 {
		log.Error("usage: workers <job>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "job", os.Args[1], "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, job string, log *slog.Logger) error {
	shutdownTracing, err := observability.InitTracing(ctx, "ticket-worker-"+job)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()
	pcfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	pcfg.ConnConfig.Tracer = observability.NewDBTracer()
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	observability.RegisterPoolMetrics(pool)
	log = log.With("job", job)
	serveMetrics(ctx, log)

	switch job {
	case "sweeper":
		svc := booking.New(pool, nil, nil, 0)
		return every(ctx, envDuration("SWEEP_INTERVAL", 15*time.Second), log, func(ctx context.Context) error {
			// Drain in batches so one tick catches up after downtime.
			for {
				n, err := svc.ExpireHolds(ctx, 500)
				if err != nil {
					return err
				}
				if n > 0 {
					log.Info("expired holds", "count", n)
				}
				if n < 500 {
					break
				}
			}
			// Idempotency keys live 24 hours; the sweeper clears them out too.
			n, err := idempotency.DeleteExpired(ctx, pool)
			if n > 0 {
				log.Info("deleted expired idempotency keys", "count", n)
			}
			return err
		})
	case "availability":
		rdb, err := redisFromEnv()
		if err != nil {
			return err
		}
		defer func() { _ = rdb.Close() }()
		c := cache.New(rdb, inventory.New(pool), log)
		return every(ctx, envDuration("AVAILABILITY_INTERVAL", time.Minute), log, func(ctx context.Context) error {
			n, err := c.RecomputeAvailability(ctx)
			if err == nil {
				log.Info("recomputed availability", "events", n)
			}
			return err
		})
	case "reconciler":
		pay := payments.NewProductionClient(os.Getenv("PAYMENTS_URL"), 3*time.Second, nil)
		svc := booking.New(pool, pay, auth.NewTicketSigner([]byte(os.Getenv("TICKET_SIGNING_KEY"))), 0)
		return every(ctx, envDuration("RECONCILE_INTERVAL", 15*time.Second), log, func(ctx context.Context) error {
			// Only orders untouched for 30s: younger ones may still be mid-checkout.
			r, err := svc.Reconcile(ctx, 30*time.Second, 200)
			metrics.Reconciled.WithLabelValues("confirmed").Add(float64(r.Confirmed))
			metrics.Reconciled.WithLabelValues("failed").Add(float64(r.Failed))
			metrics.Reconciled.WithLabelValues("refunded").Add(float64(r.Refunded))
			if r != (booking.ReconcileResult{}) {
				log.Info("reconciled", "confirmed", r.Confirmed, "failed", r.Failed,
					"still_pending", r.StillPending, "refunded", r.Refunded)
			}
			return err
		})
	case "outbox-relay":
		rdb, err := redisFromEnv()
		if err != nil {
			return err
		}
		defer func() { _ = rdb.Close() }()
		relay := outbox.NewRelay(pool, rdb)
		return every(ctx, envDuration("RELAY_INTERVAL", 500*time.Millisecond), log, func(ctx context.Context) error {
			for {
				n, err := relay.PublishBatch(ctx, 500)
				metrics.OutboxPublished.Add(float64(n))
				if err != nil || n < 500 {
					if lag, lerr := relay.Lag(ctx); lerr == nil {
						metrics.OutboxLag.Set(lag.Seconds())
					}
					return err
				}
			}
		})
	case "notifier":
		rdb, err := redisFromEnv()
		if err != nil {
			return err
		}
		defer func() { _ = rdb.Close() }()
		host, _ := os.Hostname()
		c := outbox.NewConsumer(rdb, "notifier", host, outbox.Once(rdb, "notifier", func(ctx context.Context, e outbox.Event) error {
			// Out of scope per the spec: notifications are logged, not emailed.
			log.InfoContext(ctx, "notification sent", "type", e.Type, "event_id", e.ID,
				"order_id", e.Payload["order_id"], "user_id", e.Payload["user_id"])
			metrics.Notifications.WithLabelValues(e.Type).Inc()
			return nil
		}), log)
		if err := c.Ensure(ctx); err != nil {
			return err
		}
		log.Info("started")
		for ctx.Err() == nil {
			if _, err := c.Poll(ctx, 100, 2*time.Second); err != nil && ctx.Err() == nil {
				log.Error("poll failed", "err", err)
				time.Sleep(time.Second)
			}
		}
		return nil
	case "invariants":
		svc := booking.New(pool, nil, nil, 0)
		return every(ctx, envDuration("INVARIANT_INTERVAL", 30*time.Second), log, func(ctx context.Context) error {
			v, err := svc.CheckInvariants(ctx)
			if err != nil {
				return err
			}
			for check, n := range map[string]int{
				"seats_with_multiple_valid_tickets":      v.SeatsWithMultipleValidTickets,
				"sold_seats_without_confirmed_order":     v.SoldSeatsWithoutConfirmedSale,
				"confirmed_orders_ticket_mismatch":       v.ConfirmedOrdersTicketMismatch,
				"confirmed_orders_not_paid_exactly_once": v.ConfirmedOrdersNotPaidOnce,
				"held_seats_without_active_hold":         v.HeldSeatsWithoutActiveHold,
			} {
				metrics.InvariantViolations.WithLabelValues(check).Set(float64(n))
			}
			if v.Total() > 0 {
				log.ErrorContext(ctx, "INVARIANT VIOLATED", "violations", v)
			}
			active, err := svc.ActiveHolds(ctx)
			if err == nil {
				metrics.ActiveHolds.Set(float64(active))
			}
			return err
		})
	case "admitter":
		rdb, err := redisFromEnv()
		if err != nil {
			return err
		}
		defer func() { _ = rdb.Close() }()
		secret := []byte(os.Getenv("JWT_SECRET"))
		if len(secret) < 32 {
			return errors.New("JWT_SECRET must be at least 32 bytes")
		}
		room := waitingroom.New(rdb, auth.NewPassIssuer(secret))
		inv := inventory.New(pool)
		// Tick every second; each event's own interval is enforced inside
		// AdmitNext by a Redis gate, so extra replicas can't admit faster. If Redis
		// is down, AdmitNext fails and admission simply pauses: nobody new gets in.
		return every(ctx, time.Second, log, func(ctx context.Context) error {
			events, err := inv.QueuedEvents(ctx)
			if err != nil {
				return err
			}
			for _, ev := range events {
				n, err := room.AdmitNext(ctx, ev.ID, waitingroom.Rate{Batch: ev.Batch, Interval: ev.Interval})
				if err != nil {
					return err
				}
				left, _ := room.Length(ctx, ev.ID)
				metrics.QueueLength.WithLabelValues(ev.ID.String()).Set(float64(left))
				if n > 0 {
					metrics.Admitted.Add(float64(n))
					log.Info("admitted batch", "event_id", ev.ID, "admitted", n, "waiting", left)
				}
			}
			return nil
		})
	default:
		return fmt.Errorf("unknown job %q", job)
	}
}

// every runs fn now and then on each tick until ctx is cancelled. A failed run is
// logged and retried on the next tick rather than crashing the worker.
func every(ctx context.Context, interval time.Duration, log *slog.Logger, fn func(context.Context) error) error {
	log.Info("started", "interval", interval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			log.Error("run failed", "err", err)
		}
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return nil
		case <-t.C:
		}
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func redisFromEnv() (*redis.Client, error) {
	opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL: %w", err)
	}
	return redis.NewClient(opt), nil
}

// serveMetrics exposes /metrics and /healthz for the job until ctx ends.
func serveMetrics(ctx context.Context, log *slog.Logger) {
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
}
