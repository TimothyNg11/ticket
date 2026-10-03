package auth

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each Argon2id hash allocates 19 MiB. Unbounded, a burst of logins multiplies
// that until the pod is OOM-killed (found on the kind cluster in Phase 7), so
// concurrent hashing is capped.
func TestHashingConcurrencyIsBounded(t *testing.T) {
	SetHashConcurrency(2)
	defer SetHashConcurrency(DefaultHashConcurrency)

	hash, err := HashPassword("pw")
	require.NoError(t, err)
	peak.Store(0)

	var wg sync.WaitGroup
	for range 12 {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = HashPassword("pw") }()
		go func() { defer wg.Done(); _, _ = VerifyPassword("pw", hash) }()
	}
	wg.Wait()
	assert.LessOrEqual(t, peak.Load(), int64(2), "never more than the limit at once")
	assert.Equal(t, int64(2), peak.Load(), "and the limit is actually used")
}
