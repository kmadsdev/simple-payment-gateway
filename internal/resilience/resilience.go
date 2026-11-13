// Package resilience has the small tools the gateway uses around calls
// that can fail: exponential backoff with full jitter, a retry loop with
// an attempt and time budget, and a consecutive-failure circuit breaker.
package resilience

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// Backoff computes exponential delays with full jitter:
// delay = random(0, min(Max, Base * 2^attempt)).
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Ceiling returns the upper bound of the delay for attempt (0-based).
func (b Backoff) Ceiling(attempt int) time.Duration {
	d := b.Base
	for i := 0; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	return d
}

// Delay returns a jittered delay for attempt (0-based).
func (b Backoff) Delay(attempt int) time.Duration {
	c := b.Ceiling(attempt)
	if c <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(c) + 1))
}

// Retry runs fn up to attempts times. It stops early when fn succeeds,
// when retryable(err) is false, or when ctx ends (the time budget).
// It returns the last error.
func Retry(ctx context.Context, attempts int, b Backoff, retryable func(error) bool, fn func(ctx context.Context) error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(ctx); err == nil || !retryable(err) {
			return err
		}
		if i == attempts-1 {
			break
		}
		t := time.NewTimer(b.Delay(i))
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
	return err
}

// State of a circuit breaker.
type State int

// Breaker states.
const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	return [...]string{"closed", "open", "half_open"}[s]
}

// ErrOpen is returned by Allow while the breaker is open.
var ErrOpen = errors.New("circuit breaker is open")

// Breaker opens after Threshold consecutive failures. After Cooldown it
// lets one trial call through (half-open). A success closes it, a failure
// opens it again.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	Now       func() time.Time
	OnChange  func(State)

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	trial    bool
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Breaker) set(s State) {
	if b.state != s {
		b.state = s
		if b.OnChange != nil {
			b.OnChange(s)
		}
	}
}

// Allow returns nil when a call may proceed.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.Cooldown {
			return ErrOpen
		}
		b.set(HalfOpen)
		b.trial = true
		return nil
	case HalfOpen:
		if b.trial {
			return ErrOpen // one trial call at a time
		}
		b.trial = true
		return nil
	default:
		return nil
	}
}

// Success records a good call.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.trial = false
	b.set(Closed)
}

// Failure records a failed call.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trial = false
	b.failures++
	if b.state == HalfOpen || b.failures >= b.Threshold {
		b.openedAt = b.now()
		b.set(Open)
	}
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
