package web

import (
	"sync"
	"time"
)

// attemptLimiter is a fixed-window counter per key. It guards the claim endpoint against
// hammering; the payout limits themselves live in the store.
type attemptLimiter struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	now       func() time.Time
	buckets   map[string]*attemptBucket
	lastSweep time.Time
}

type attemptBucket struct {
	start time.Time
	n     int
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, now: time.Now, buckets: map[string]*attemptBucket{}}
}

// allow records an attempt by key and reports whether it is within the limit; when it
// is not, retry is how long until the key's window resets.
func (l *attemptLimiter) allow(key string) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) >= l.window {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b := l.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		b = &attemptBucket{start: now}
		l.buckets[key] = b
	}
	if b.n >= l.max {
		return false, b.start.Add(l.window).Sub(now)
	}
	b.n++
	return true, 0
}
