package web

import "time"

var (
	ClientAddr  = clientAddr
	MaskAddress = maskAddress
)

// NewAttemptLimiter returns an attempt limiter driven by now, for tests.
func NewAttemptLimiter(max int, window time.Duration, now func() time.Time) func(string) (bool, time.Duration) {
	l := newAttemptLimiter(max, window)
	l.now = now
	return l.allow
}
