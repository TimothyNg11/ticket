// Command workers runs background jobs. Each job is a subcommand so it can be
// deployed and scaled on its own:
//
//	workers sweeper        release expired holds every SWEEP_INTERVAL (default 15s)
//	workers availability   recount seats per on-sale event into Redis every minute,
//	                       correcting drift in the cached availability counters
//	workers admitter       admit the next batch from each event's waiting room,
//	                       once per the event's admission interval
//
// Every job is safe to run as several replicas at once: work is claimed with
// FOR UPDATE SKIP LOCKED, so replicas never process the same row.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"ticket/internal/auth"
	"ticket/internal/booking"
	"ticket/internal/cache"
	"ticket/internal/inventory"
	"ticket/internal/waitingroom"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	log = log.With("job", job)

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
					return nil
				}
			}
		})
	case "availability":
		ropt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
		if err != nil {
			return fmt.Errorf("REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(ropt)
		defer func() { _ = rdb.Close() }()
		c := cache.New(rdb, inventory.New(pool), log)
		return every(ctx, envDuration("AVAILABILITY_INTERVAL", time.Minute), log, func(ctx context.Context) error {
			n, err := c.RecomputeAvailability(ctx)
			if err == nil {
				log.Info("recomputed availability", "events", n)
			}
			return err
		})
	case "admitter":
		ropt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
		if err != nil {
			return fmt.Errorf("REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(ropt)
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
				if n > 0 {
					left, _ := room.Length(ctx, ev.ID)
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
