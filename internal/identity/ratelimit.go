package identity

import (
	"sync"
	"time"
)

// RateLimiter throttles repeated attempts from one source.
//
// It counts failures only: a correct password is never refused, so a user who
// mistypes twice and then gets it right is not locked out, and the limiter still
// stops a run of guesses. This is the "failure-only throttling" behaviour in
// docs/parity-checklist.md.
//
// The counters are process-local. A multi-node deployment would need a shared
// store, which the architecture defers until there is more than one node
// (docs/architecture.md section 6).
type RateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	attempts map[string]*attemptWindow
	now      func() time.Time
}

type attemptWindow struct {
	count int
	start time.Time
}

// NewRateLimiter allows limit failures per window per source. A limit of zero or
// less disables limiting, which keeps a call site free of conditionals.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:    limit,
		window:   window,
		attempts: map[string]*attemptWindow{},
		now:      time.Now,
	}
}

// Blocked reports whether source has used up its failures in the current
// window. It records nothing, so the caller decides what counts as a failure.
func (l *RateLimiter) Blocked(source string) bool {
	if l.disabled() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	window, found := l.attempts[source]
	if !found || l.now().Sub(window.start) >= l.window {
		return false
	}
	return window.count >= l.limit
}

// Record counts one failure for source.
func (l *RateLimiter) Record(source string) {
	if l.disabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	window, found := l.attempts[source]
	if !found || now.Sub(window.start) >= l.window {
		l.sweep(now)
		l.attempts[source] = &attemptWindow{count: 1, start: now}
		return
	}
	window.count++
}

// Reset clears the failures for one source. A successful sign-in calls it, so a
// legitimate user is not throttled by an earlier burst.
func (l *RateLimiter) Reset(source string) {
	if l == nil || source == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, source)
}

func (l *RateLimiter) disabled() bool {
	return l == nil || l.limit <= 0 || l.window <= 0
}

// sweep drops windows that have expired, so the map cannot grow without bound
// from one-off sources. It is called while recording an attempt, which already
// holds the lock.
func (l *RateLimiter) sweep(now time.Time) {
	for source, window := range l.attempts {
		if now.Sub(window.start) >= l.window {
			delete(l.attempts, source)
		}
	}
}
