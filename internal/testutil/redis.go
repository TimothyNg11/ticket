package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// One Redis container per test binary, started on first use. Tests share it, so
// they must use keys that can't collide (event and user ids are UUIDs, which
// makes that natural).
var (
	redisOnce sync.Once
	redisCtr  *tcredis.RedisContainer
	redisURL  string
	redisErr  error
)

// NewRedis returns a client for the shared test Redis, starting it if needed.
// It skips the test under -short.
func NewRedis(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs Docker; run without -short")
	}
	redisOnce.Do(func() {
		pinDockerHost()
		ctx := context.Background()
		for attempt := 1; attempt <= 4; attempt++ {
			if redisCtr, redisErr = tcredis.Run(ctx, "redis:7-alpine"); redisErr == nil {
				redisURL, redisErr = redisCtr.ConnectionString(ctx)
				return
			}
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	})
	if redisErr != nil {
		t.Fatalf("start redis: %v", redisErr)
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// RedisURL returns the shared test Redis URL (after NewRedis has started it).
func RedisURL() string { return redisURL }

func stopRedis() {
	if redisCtr != nil {
		if err := redisCtr.Terminate(context.Background()); err != nil {
			fmt.Println("testutil: stop redis:", err)
		}
	}
}
