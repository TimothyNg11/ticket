// Package waitingroom is the virtual queue in front of high-demand events.
//
// When 50,000 people arrive for 5,000 seats, letting everyone at the seat map
// and hold endpoints at once would hand the database 50,000 concurrent lock
// fights. Instead, buyers join a first-come-first-served queue (a Redis sorted
// set scored by arrival time) and an admitter lets them through in fixed batches.
// Only admitted buyers get an admission pass, and only a pass lets you hold seats,
// so the database sees at most one batch of buyers at a time.
package waitingroom

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"ticket/internal/auth"
)

// AdmissionTTL is how long an admitted buyer has to pick and hold seats.
const AdmissionTTL = 15 * time.Minute

// queueTTL bounds how long a queue position token stays valid.
const queueTTL = 24 * time.Hour

func queueKey(event uuid.UUID) string { return "queue:" + event.String() }
func admittedKey(event, user uuid.UUID) string {
	return "admitted:" + event.String() + ":" + user.String()
}
func gateKey(event uuid.UUID) string { return "admit-gate:" + event.String() }

// join adds the user scored by the Redis server's clock (milliseconds), but only
// if they aren't queued already: rejoining keeps your original place. Returns the
// 0-based rank.
var join = redis.NewScript(`
local t = redis.call('TIME')
local ms = t[1] * 1000 + math.floor(t[2] / 1000)
redis.call('ZADD', KEYS[1], 'NX', ms, ARGV[1])
return redis.call('ZRANK', KEYS[1], ARGV[1])
`)

// Room manages queues for every event.
type Room struct {
	rdb    *redis.Client
	passes *auth.PassIssuer
}

// New returns a Room.
func New(rdb *redis.Client, passes *auth.PassIssuer) *Room { return &Room{rdb: rdb, passes: passes} }

// Position is where a buyer stands.
type Position struct {
	QueueToken string
	// Position is 1-based; 0 once admitted.
	Position int
	// EstimatedWait assumes the event's admission rate holds.
	EstimatedWait time.Duration
	Admitted      bool
	// AdmissionToken and AdmissionExpires are set once admitted.
	AdmissionToken   string
	AdmissionExpires time.Time
}

// Rate describes how fast an event's queue drains.
type Rate struct {
	Batch    int
	Interval time.Duration
}

func (r Rate) wait(position int) time.Duration {
	if r.Batch <= 0 {
		return 0
	}
	return time.Duration(math.Ceil(float64(position)/float64(r.Batch))) * r.Interval
}

// ErrNotQueued means the user has neither a queue place nor an admission.
var ErrNotQueued = errors.New("waitingroom: not in queue")

// Join puts the user in the event's queue (or reports their existing place).
func (w *Room) Join(ctx context.Context, eventID, userID uuid.UUID, rate Rate) (Position, error) {
	tok, _, err := w.passes.Issue(auth.TypeQueue, userID, eventID, queueTTL)
	if err != nil {
		return Position{}, err
	}
	if p, ok, err := w.admitted(ctx, eventID, userID); err != nil || ok {
		p.QueueToken = tok
		return p, err
	}
	rank, err := join.Run(ctx, w.rdb, []string{queueKey(eventID)}, userID.String()).Int()
	if err != nil {
		return Position{}, err
	}
	return Position{QueueToken: tok, Position: rank + 1, EstimatedWait: rate.wait(rank + 1)}, nil
}

// Status reports the progress of the holder of a queue token.
func (w *Room) Status(ctx context.Context, queueToken string, rate func(eventID uuid.UUID) Rate) (auth.EventPass, Position, error) {
	pass, err := w.passes.Verify(auth.TypeQueue, queueToken)
	if err != nil {
		return pass, Position{}, err
	}
	if p, ok, err := w.admitted(ctx, pass.EventID, pass.UserID); err != nil || ok {
		p.QueueToken = queueToken
		return pass, p, err
	}
	rank, err := w.rdb.ZRank(ctx, queueKey(pass.EventID), pass.UserID.String()).Result()
	if errors.Is(err, redis.Nil) {
		return pass, Position{}, ErrNotQueued // admission expired, or never joined
	}
	if err != nil {
		return pass, Position{}, err
	}
	pos := int(rank) + 1
	return pass, Position{QueueToken: queueToken, Position: pos, EstimatedWait: rate(pass.EventID).wait(pos)}, nil
}

func (w *Room) admitted(ctx context.Context, eventID, userID uuid.UUID) (Position, bool, error) {
	tok, err := w.rdb.Get(ctx, admittedKey(eventID, userID)).Result()
	if errors.Is(err, redis.Nil) {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, err
	}
	pass, err := w.passes.Verify(auth.TypeAdmission, tok)
	if err != nil {
		return Position{}, false, nil
	}
	return Position{Admitted: true, AdmissionToken: tok, AdmissionExpires: pass.ExpiresAt}, true, nil
}

// AdmitNext admits up to rate.Batch buyers from the front of the queue, at most
// once per rate.Interval across all admitter replicas. It returns how many were
// admitted (0 if another replica already admitted this interval).
func (w *Room) AdmitNext(ctx context.Context, eventID uuid.UUID, rate Rate) (int, error) {
	// The gate key is the cross-replica schedule: whoever sets it admits a batch,
	// and nobody can admit again until it expires one interval later.
	ok, err := w.rdb.SetNX(ctx, gateKey(eventID), 1, rate.Interval).Result()
	if err != nil || !ok {
		return 0, err
	}
	popped, err := w.rdb.ZPopMin(ctx, queueKey(eventID), int64(rate.Batch)).Result()
	if err != nil {
		return 0, err
	}
	pipe := w.rdb.Pipeline()
	for _, z := range popped {
		user, err := uuid.Parse(fmt.Sprint(z.Member))
		if err != nil {
			continue
		}
		tok, _, err := w.passes.Issue(auth.TypeAdmission, user, eventID, AdmissionTTL)
		if err != nil {
			return 0, err
		}
		pipe.Set(ctx, admittedKey(eventID, user), tok, AdmissionTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return len(popped), nil
}

// Length is the number of buyers still waiting.
func (w *Room) Length(ctx context.Context, eventID uuid.UUID) (int64, error) {
	return w.rdb.ZCard(ctx, queueKey(eventID)).Result()
}

// CheckAdmission verifies an admission pass for this user and event. Passes are
// bound to both, so one can't be shared with a friend or reused for another event.
func (w *Room) CheckAdmission(token string, userID, eventID uuid.UUID) error {
	pass, err := w.passes.Verify(auth.TypeAdmission, token)
	if err != nil || pass.UserID != userID || pass.EventID != eventID {
		return auth.ErrInvalidToken
	}
	return nil
}
