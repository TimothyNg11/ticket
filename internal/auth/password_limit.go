package auth

import (
	"sync"
	"sync/atomic"
)

// DefaultHashConcurrency is how many Argon2id computations may run at once per
// process. Each one allocates 19 MiB, so 4 bounds hashing to about 76 MiB.
const DefaultHashConcurrency = 4

var (
	slotsMu sync.RWMutex
	slots   = make(chan struct{}, DefaultHashConcurrency)

	inFlight atomic.Int64 // current hashes running
	peak     atomic.Int64 // highest inFlight seen (tests and metrics)
)

// SetHashConcurrency changes the limit. Call it at startup, before serving.
func SetHashConcurrency(n int) {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	slots = make(chan struct{}, max(n, 1))
}

// withHashSlot runs fn once a slot is free. Bursts of logins therefore queue for
// a few milliseconds instead of multiplying memory until the pod is OOM-killed.
func withHashSlot(fn func()) {
	slotsMu.RLock()
	s := slots
	slotsMu.RUnlock()
	s <- struct{}{}
	defer func() { <-s }()
	n := inFlight.Add(1)
	defer inFlight.Add(-1)
	for {
		p := peak.Load()
		if n <= p || peak.CompareAndSwap(p, n) {
			break
		}
	}
	fn()
}

// HashesInFlight reports how many password hashes are running right now.
func HashesInFlight() int64 { return inFlight.Load() }
