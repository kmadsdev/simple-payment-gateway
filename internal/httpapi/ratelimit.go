package httpapi

import (
	"sync"
	"time"
)

// RateLimiter is a per-key limiter based on GCRA (generic cell rate
// algorithm). It keeps one timestamp per key and uses only integer time
// math. It allows `burst` requests at once and `rate` per second on average.
//
// State is in memory, so each replica limits on its own. A shared store
// (Redis) would be needed for a global limit; see docs/adr/0009.
type RateLimiter struct {
	interval time.Duration // time between two requests at the steady rate
	burst    time.Duration // interval * burst: how far ahead the schedule may run
	now      func() time.Time

	mu  sync.Mutex
	tat map[string]time.Time // theoretical arrival time per key
}

// NewRateLimiter returns a limiter of rate requests per second with burst.
func NewRateLimiter(rate, burst int, now func() time.Time) *RateLimiter {
	if rate < 1 {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}
	if now == nil {
		now = time.Now
	}
	iv := time.Second / time.Duration(rate)
	return &RateLimiter{interval: iv, burst: iv * time.Duration(burst), now: now, tat: map[string]time.Time{}}
}

// Allow reports whether a request for key may pass. When not, it returns
// how long to wait.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	tat := l.tat[key]
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(l.interval)
	if ahead := next.Sub(now); ahead > l.burst {
		return false, ahead - l.burst
	}
	l.tat[key] = next
	return true, 0
}
