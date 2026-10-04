package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// DBHealth tracks whether Postgres is reachable, for the readiness probe.
//
// It checks on its own dedicated connection, never the request pool: a pool
// saturated by load is not an outage, and if readiness waited on that pool,
// every API pod would fail its check at the same moment and the load balancer
// would drop all of them, turning "slow" into "down" (Phase 9 hit exactly this).
// It also only reports unready after the database has been unreachable for a
// sustained period, so a blip doesn't pull a pod out of service.
type DBHealth struct {
	url       string
	tolerance time.Duration
	now       func() time.Time

	mu       sync.Mutex
	lastOK   time.Time
	lastErr  error
	conn     *pgx.Conn
	interval time.Duration
}

// NewDBHealth returns a checker for databaseURL that tolerates failures for up
// to tolerance before reporting unready.
func NewDBHealth(databaseURL string, tolerance time.Duration) *DBHealth {
	return &DBHealth{url: databaseURL, tolerance: tolerance, now: time.Now, lastOK: time.Now(), interval: 2 * time.Second}
}

// Run checks every couple of seconds until ctx is cancelled.
func (h *DBHealth) Run(ctx context.Context) {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		h.record(h.ping(ctx))
		select {
		case <-ctx.Done():
			if h.conn != nil {
				_ = h.conn.Close(context.Background())
			}
			return
		case <-t.C:
		}
	}
}

func (h *DBHealth) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if h.conn == nil || h.conn.IsClosed() {
		c, err := pgx.Connect(ctx, h.url)
		if err != nil {
			return err
		}
		h.conn = c
	}
	if err := h.conn.Ping(ctx); err != nil {
		_ = h.conn.Close(context.Background())
		h.conn = nil
		return err
	}
	return nil
}

func (h *DBHealth) record(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastErr = err
	if err == nil {
		h.lastOK = h.now()
	}
}

// Ready returns an error once the database has been unreachable for longer
// than the tolerance.
func (h *DBHealth) Ready() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastErr != nil && h.now().Sub(h.lastOK) > h.tolerance {
		return errors.Join(errors.New("database unreachable"), h.lastErr)
	}
	return nil
}
