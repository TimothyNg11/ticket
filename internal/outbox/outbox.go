// Package outbox moves domain events from the Postgres outbox table onto a Redis
// Stream, and consumes them.
//
// Delivery is at-least-once: the relay publishes and then marks rows published,
// so a crash between the two re-publishes a batch. Consumers therefore treat
// events idempotently, deduplicating on each event's id.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"ticket/internal/db"
	"ticket/internal/db/sqlc"
)

// Stream is the Redis Stream all domain events are published to.
const Stream = "events"

// Relay publishes unpublished outbox rows.
type Relay struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
}

// NewRelay returns a Relay.
func NewRelay(pool *pgxpool.Pool, rdb *redis.Client) *Relay { return &Relay{pool: pool, rdb: rdb} }

// PublishBatch publishes up to n events and returns how many it sent. The rows
// stay locked (SKIP LOCKED) until the transaction commits, so concurrent relays
// never send the same row at the same time.
func (r *Relay) PublishBatch(ctx context.Context, n int) (int, error) {
	sent := 0
	err := db.InTx(ctx, r.pool, func(q *sqlc.Queries) error {
		rows, err := q.ClaimOutbox(ctx, int32(min(n, 10_000))) //nolint:gosec // clamped
		if err != nil || len(rows) == 0 {
			return err
		}
		pipe := r.rdb.Pipeline()
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
			pipe.XAdd(ctx, &redis.XAddArgs{
				Stream: Stream,
				MaxLen: 1_000_000, Approx: true, // cap memory; consumers keep up long before this
				Values: map[string]any{"type": row.EventType, "aggregate_id": row.AggregateID.String(), "payload": string(row.Payload)},
			})
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err // rolls back: rows stay unpublished and are retried
		}
		// If we crash right here, the events were sent but the rows aren't marked:
		// they will be sent again. That's the at-least-once part.
		sent = len(rows)
		return q.MarkOutboxPublished(ctx, ids)
	})
	return sent, err
}

// Lag returns the age of the oldest unpublished event.
func (r *Relay) Lag(ctx context.Context) (time.Duration, error) {
	secs, err := sqlc.New(r.pool).OutboxLag(ctx)
	return time.Duration(secs * float64(time.Second)), err
}

// Event is a consumed domain event.
type Event struct {
	ID      string         // the payload's event_id, used for deduplication
	Type    string         // e.g. "order.confirmed"
	Payload map[string]any //
}

// Handler processes one event. Returning an error leaves it pending for retry.
type Handler func(ctx context.Context, e Event) error

// Consumer reads the stream as part of a consumer group, so each event goes to
// one member of the group, and acknowledges only after handling succeeds.
type Consumer struct {
	rdb       *redis.Client
	group     string
	name      string
	handle    Handler
	log       *slog.Logger
	claimIdle time.Duration
}

// NewConsumer returns a consumer in group (created if missing) named name.
func NewConsumer(rdb *redis.Client, group, name string, handle Handler, log *slog.Logger) *Consumer {
	return &Consumer{rdb: rdb, group: group, name: name, handle: handle, log: log, claimIdle: time.Minute}
}

// Ensure creates the consumer group (reading from the start of the stream).
func (c *Consumer) Ensure(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, Stream, c.group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Poll handles up to n new events, waiting up to block for some to arrive. It
// also reclaims events another consumer took but never acknowledged (it crashed)
// once they've been idle for a minute.
func (c *Consumer) Poll(ctx context.Context, n int64, block time.Duration) (int, error) {
	claimed, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: Stream, Group: c.group, Consumer: c.name, MinIdle: c.claimIdle, Start: "0", Count: n,
	}).Result()
	if err != nil {
		return 0, err
	}
	msgs := claimed
	if len(msgs) == 0 {
		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: c.group, Consumer: c.name, Streams: []string{Stream, ">"}, Count: n, Block: block,
		}).Result()
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		for _, s := range streams {
			msgs = append(msgs, s.Messages...)
		}
	}
	handled := 0
	for _, m := range msgs {
		e, err := decode(m)
		if err != nil {
			// A malformed message will never succeed; log and drop it rather than
			// blocking the stream forever.
			c.log.ErrorContext(ctx, "dropping malformed event", "stream_id", m.ID, "err", err)
		} else if err := c.handle(ctx, e); err != nil {
			c.log.WarnContext(ctx, "event handler failed; will retry", "event_id", e.ID, "type", e.Type, "err", err)
			continue
		}
		if err := c.rdb.XAck(ctx, Stream, c.group, m.ID).Err(); err != nil {
			return handled, err
		}
		handled++
	}
	return handled, nil
}

func decode(m redis.XMessage) (Event, error) {
	typ, _ := m.Values["type"].(string)
	raw, _ := m.Values["payload"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return Event{}, err
	}
	id, _ := payload["event_id"].(string)
	if typ == "" || id == "" {
		return Event{}, fmt.Errorf("missing type or event_id")
	}
	return Event{ID: id, Type: typ, Payload: payload}, nil
}

// Once wraps h so each event id is handled at most once (for 7 days), turning
// at-least-once delivery into effectively-once handling. The marker is set before
// handling; if handling then fails, the marker is removed so a retry can run.
func Once(rdb *redis.Client, scope string, h Handler) Handler {
	return func(ctx context.Context, e Event) error {
		key := "handled:" + scope + ":" + e.ID
		first, err := rdb.SetNX(ctx, key, 1, 7*24*time.Hour).Result()
		if err != nil {
			return err
		}
		if !first {
			return nil // duplicate delivery
		}
		if err := h(ctx, e); err != nil {
			rdb.Del(ctx, key)
			return err
		}
		return nil
	}
}

// SetClaimIdle overrides how long an unacknowledged event must sit idle before
// another consumer reclaims it (tests shorten it).
func SetClaimIdle(c *Consumer, d time.Duration) { c.claimIdle = d }
