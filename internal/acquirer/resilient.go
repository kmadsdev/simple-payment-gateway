package acquirer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
)

// Observer receives one call per attempt. It feeds the metrics.
type Observer func(op, outcome string, d time.Duration)

// Resilient wraps an Acquirer with a per-attempt timeout, a retry loop
// with an attempt and time budget, and a circuit breaker. Retries are safe
// because every call carries an idempotency key.
type Resilient struct {
	Inner          Acquirer
	AttemptTimeout time.Duration
	TotalBudget    time.Duration
	MaxAttempts    int
	Backoff        resilience.Backoff
	Breaker        *resilience.Breaker
	Observe        Observer
}

func (r *Resilient) call(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, r.TotalBudget)
	defer cancel()
	return resilience.Retry(ctx, r.MaxAttempts, r.Backoff, Retryable, func(ctx context.Context) error {
		if err := r.Breaker.Allow(); err != nil {
			r.observe(op, "circuit_open", 0)
			// Not retryable: no call was made, and the breaker is still open.
			return fmt.Errorf("%w: %w", errCircuitOpen, err)
		}
		actx, acancel := context.WithTimeout(ctx, r.AttemptTimeout)
		defer acancel()
		start := time.Now()
		err := fn(actx)
		if err != nil && errors.Is(actx.Err(), context.DeadlineExceeded) && !errors.Is(err, ErrTimeout) {
			err = fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		switch {
		case err == nil:
			r.Breaker.Success()
			r.observe(op, "ok", time.Since(start))
		case Retryable(err):
			r.Breaker.Failure()
			r.observe(op, "timeout", time.Since(start))
		default:
			// A business error (unknown ref) is not a provider failure.
			r.Breaker.Success()
			r.observe(op, "error", time.Since(start))
		}
		return err
	})
}

var errCircuitOpen = errors.New("circuit open")

// IsUnavailable reports whether err means the provider could not be
// reached: retries exhausted, budget spent, or circuit open.
func IsUnavailable(err error) bool {
	return Retryable(err) || errors.Is(err, errCircuitOpen) || errors.Is(err, context.DeadlineExceeded)
}

func (r *Resilient) observe(op, outcome string, d time.Duration) {
	if r.Observe != nil {
		r.Observe(op, outcome, d)
	}
}

// Authorize calls Inner.Authorize with resilience.
func (r *Resilient) Authorize(ctx context.Context, req Request) (Result, error) {
	var res Result
	err := r.call(ctx, "authorize", func(ctx context.Context) error {
		var err error
		res, err = r.Inner.Authorize(ctx, req)
		return err
	})
	return res, err
}

// Capture calls Inner.Capture with resilience.
func (r *Resilient) Capture(ctx context.Context, ref, key string, amount money.Amount) error {
	return r.call(ctx, "capture", func(ctx context.Context) error { return r.Inner.Capture(ctx, ref, key, amount) })
}

// Void calls Inner.Void with resilience.
func (r *Resilient) Void(ctx context.Context, ref, key string) error {
	return r.call(ctx, "void", func(ctx context.Context) error { return r.Inner.Void(ctx, ref, key) })
}

// Refund calls Inner.Refund with resilience.
func (r *Resilient) Refund(ctx context.Context, ref, key string, amount money.Amount) error {
	return r.call(ctx, "refund", func(ctx context.Context) error { return r.Inner.Refund(ctx, ref, key, amount) })
}

// Status calls Inner.Status with resilience.
func (r *Resilient) Status(ctx context.Context, ref string) (Result, error) {
	var res Result
	err := r.call(ctx, "status", func(ctx context.Context) error {
		var err error
		res, err = r.Inner.Status(ctx, ref)
		return err
	})
	return res, err
}
