// Package cache keeps the read storm of an on-sale away from Postgres.
//
// Everything here is cache-aside: read Redis, fall back to the inventory service
// on a miss, then fill Redis. Redis is never the authority on who owns a seat; a
// cached seat map can be up to a couple of seconds stale, and holds are always
// decided by row locks in Postgres. Any Redis failure degrades to reading
// Postgres directly instead of failing the request.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"ticket/internal/db/sqlc"
	"ticket/internal/inventory"
	"ticket/internal/metrics"
)

// TTLs from the spec's caching table.
const (
	seatMapTTL   = 2 * time.Second  // short: seat maps change constantly during a sale
	staleTTL     = 30 * time.Second // served while one request rebuilds an expired map
	eventTTL     = 5 * time.Minute
	eventListTTL = 30 * time.Second
	rebuildLock  = 2 * time.Second
)

// Stats counts cache outcomes (exported as metrics in Phase 8).
type Stats struct {
	Hits, Misses, StaleServed, Rebuilds, Errors atomic.Int64
}

// Cache serves cached inventory reads.
type Cache struct {
	rdb   *redis.Client
	inv   *inventory.Service
	sf    singleflight.Group
	log   *slog.Logger
	Stats Stats
}

// New returns a Cache over inv.
func New(rdb *redis.Client, inv *inventory.Service, log *slog.Logger) *Cache {
	return &Cache{rdb: rdb, inv: inv, log: log}
}

func seatMapVerKey(id uuid.UUID) string       { return "seatmap:" + id.String() + ":ver" }
func seatMapStaleKey(id uuid.UUID) string     { return "seatmap:" + id.String() + ":stale" }
func eventKey(id uuid.UUID) string            { return "event:" + id.String() }
func availKey(id uuid.UUID) string            { return "avail:" + id.String() }
func seatMapKey(id uuid.UUID, v int64) string { return fmt.Sprintf("seatmap:%s:v%d", id, v) }

const eventListVerKey = "events:list:ver"

// jitter spreads expiries by up to 20% so keys filled together don't all expire
// together and stampede the database at the same instant.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d)/5+1)) //nolint:gosec // jitter needs no cryptographic randomness
}

func (c *Cache) fail(ctx context.Context, op string, err error) {
	c.Stats.Errors.Add(1)
	metrics.CacheLookups.WithLabelValues(op, "error").Inc()
	c.log.WarnContext(ctx, "cache unavailable, reading from database", "op", op, "err", err)
}

// SeatMap returns an event's seat map.
//
// The key carries a version number that every seat change bumps
// (seatmap:{event}:v{n}), so invalidation is one INCR and no reader can see a map
// older than the last change plus the 2-second TTL. When a hot key is missing,
// only one request per pod (singleflight) and, via a short Redis lock, one pod
// overall rebuilds it; the rest serve the previous version ("stale") meanwhile.
func (c *Cache) SeatMap(ctx context.Context, eventID uuid.UUID) (inventory.SeatMap, error) {
	ver, err := c.rdb.Get(ctx, seatMapVerKey(eventID)).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		c.fail(ctx, "seatmap", err)
		return c.inv.SeatMap(ctx, eventID)
	}
	key := seatMapKey(eventID, ver)
	if sm, ok := c.getSeatMap(ctx, key); ok {
		c.Stats.Hits.Add(1)
		metrics.CacheLookups.WithLabelValues("seatmap", "hit").Inc()
		return sm, nil
	}
	c.Stats.Misses.Add(1)
	metrics.CacheLookups.WithLabelValues("seatmap", "miss").Inc()

	v, err, _ := c.sf.Do(key, func() (any, error) {
		// Check again: a request that missed just before another flight finished
		// would otherwise start a second flight and rebuild a key that's now filled.
		if sm, ok := c.getSeatMap(ctx, key); ok {
			return sm, nil
		}
		got, err := c.rdb.SetNX(ctx, "lock:"+key, 1, rebuildLock).Result()
		if err == nil && !got {
			// Another pod is rebuilding: serve the last known map if there is one.
			if sm, ok := c.getSeatMap(ctx, seatMapStaleKey(eventID)); ok {
				c.Stats.StaleServed.Add(1)
				metrics.CacheLookups.WithLabelValues("seatmap", "stale").Inc()
				return sm, nil
			}
		}
		c.Stats.Rebuilds.Add(1)
		sm, err := c.inv.SeatMap(ctx, eventID)
		if err != nil {
			return nil, err
		}
		if b, err := json.Marshal(sm); err == nil {
			pipe := c.rdb.Pipeline()
			pipe.Set(ctx, key, b, jitter(seatMapTTL))
			pipe.Set(ctx, seatMapStaleKey(eventID), b, staleTTL)
			pipe.Del(ctx, "lock:"+key)
			if _, err := pipe.Exec(ctx); err != nil {
				c.fail(ctx, "seatmap fill", err)
			}
		}
		return sm, nil
	})
	if err != nil {
		return inventory.SeatMap{}, err
	}
	return v.(inventory.SeatMap), nil
}

func (c *Cache) getSeatMap(ctx context.Context, key string) (inventory.SeatMap, bool) {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return inventory.SeatMap{}, false
	}
	var sm inventory.SeatMap
	return sm, json.Unmarshal(b, &sm) == nil
}

// SeatsChanged is the booking hook: it invalidates the event's seat map and
// adjusts its availability counter. Called after the change has committed.
func (c *Cache) SeatsChanged(eventID uuid.UUID, availableDelta int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pipe := c.rdb.Pipeline()
	pipe.Incr(ctx, seatMapVerKey(eventID))
	if availableDelta != 0 {
		// Only adjust a counter that exists; a missing one is rebuilt from Postgres.
		pipe.Eval(ctx, `if redis.call('EXISTS', KEYS[1]) == 1 then return redis.call('INCRBY', KEYS[1], ARGV[1]) end return nil`,
			[]string{availKey(eventID)}, availableDelta)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		c.fail(ctx, "invalidate", err)
	}
}

// Event returns a published event, cached for five minutes.
func (c *Cache) Event(ctx context.Context, id uuid.UUID) (sqlc.Event, error) {
	var e sqlc.Event
	if b, err := c.rdb.Get(ctx, eventKey(id)).Bytes(); err == nil && json.Unmarshal(b, &e) == nil {
		c.Stats.Hits.Add(1)
		return e, nil
	} else if err != nil && !errors.Is(err, redis.Nil) {
		c.fail(ctx, "event", err)
		return c.inv.GetEvent(ctx, id)
	}
	c.Stats.Misses.Add(1)
	e, err := c.inv.GetEvent(ctx, id)
	if err != nil {
		return e, err // not-found and drafts aren't cached
	}
	if b, err := json.Marshal(e); err == nil {
		c.rdb.Set(ctx, eventKey(id), b, jitter(eventTTL))
	}
	return e, nil
}

type eventPage struct {
	Events []sqlc.Event `json:"events"`
	Next   string       `json:"next"`
}

// ListEvents returns a page of published events, cached briefly. Pages are keyed
// by a list version that any event change bumps.
func (c *Cache) ListEvents(ctx context.Context, cursor string, limit int) ([]sqlc.Event, string, error) {
	ver, err := c.rdb.Get(ctx, eventListVerKey).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		c.fail(ctx, "events", err)
		return c.inv.ListEvents(ctx, cursor, limit)
	}
	key := fmt.Sprintf("events:list:v%d:%d:%s", ver, limit, cursor)
	var p eventPage
	if b, err := c.rdb.Get(ctx, key).Bytes(); err == nil && json.Unmarshal(b, &p) == nil {
		c.Stats.Hits.Add(1)
		return p.Events, p.Next, nil
	}
	c.Stats.Misses.Add(1)
	events, next, err := c.inv.ListEvents(ctx, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	if b, err := json.Marshal(eventPage{events, next}); err == nil {
		c.rdb.Set(ctx, key, b, jitter(eventListTTL))
	}
	return events, next, nil
}

// EventChanged drops an event's cached details and all cached list pages.
func (c *Cache) EventChanged(ctx context.Context, id uuid.UUID) {
	pipe := c.rdb.Pipeline()
	pipe.Del(ctx, eventKey(id))
	pipe.Incr(ctx, eventListVerKey)
	if _, err := pipe.Exec(ctx); err != nil {
		c.fail(ctx, "event invalidate", err)
	}
}

// Available returns the number of seats still available for an event. The
// counter lives in Redis and is adjusted on every seat change; when it's missing
// it is recounted from Postgres.
func (c *Cache) Available(ctx context.Context, eventID uuid.UUID) (int, error) {
	n, err := c.rdb.Get(ctx, availKey(eventID)).Int()
	if err == nil {
		c.Stats.Hits.Add(1)
		return n, nil
	}
	if !errors.Is(err, redis.Nil) {
		c.fail(ctx, "available", err)
		return c.inv.CountAvailable(ctx, eventID)
	}
	c.Stats.Misses.Add(1)
	n, err = c.inv.CountAvailable(ctx, eventID)
	if err != nil {
		return 0, err
	}
	c.rdb.SetNX(ctx, availKey(eventID), n, 0)
	return n, nil
}

// RecomputeAvailability overwrites every on-sale event's counter with the true
// count from Postgres, correcting any drift (e.g. a lost INCRBY during a Redis
// blip). Run periodically by the workers binary.
func (c *Cache) RecomputeAvailability(ctx context.Context) (int, error) {
	ids, err := c.inv.OnSaleEventIDs(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		n, err := c.inv.CountAvailable(ctx, id)
		if err != nil {
			return 0, err
		}
		if err := c.rdb.Set(ctx, availKey(id), strconv.Itoa(n), 0).Err(); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Revoke marks an access token as revoked until it would have expired anyway.
func (c *Cache) Revoke(ctx context.Context, jti string, until time.Time) error {
	ttl := time.Until(until)
	if ttl <= 0 {
		return nil
	}
	return c.rdb.Set(ctx, "revoked:"+jti, 1, ttl).Err()
}

// IsRevoked reports whether an access token was revoked. If Redis is down it
// reports false: tokens live 15 minutes, and refusing every request during an
// outage would turn a cache failure into a full outage.
func (c *Cache) IsRevoked(ctx context.Context, jti string) bool {
	n, err := c.rdb.Exists(ctx, "revoked:"+jti).Result()
	if err != nil {
		c.fail(ctx, "revoked", err)
		return false
	}
	return n == 1
}
