package service

import (
	"context"
	"errors"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// MaintenanceOnce runs one pass of the background work:
//
//   - poll the provider for pending payments (delayed approval, paid Pix)
//   - expire uncaptured authorizations and unpaid pending charges
//   - fail operations stuck in pending after a crash
//   - purge old idempotency keys
//
// Errors on one payment are logged and do not stop the pass.
func (s *Service) MaintenanceOnce(ctx context.Context) {
	const batch = 100
	now := s.now()

	if ids, err := s.Store.PendingAtProvider(ctx, batch); err != nil {
		s.Log.ErrorContext(ctx, "list pending payments", "error", err)
	} else {
		for _, pid := range ids {
			if _, err := s.SyncWithProvider(ctx, pid); err != nil {
				s.Log.WarnContext(ctx, "sync with provider", "payment_id", pid, "error", err)
			}
		}
	}

	if ids, err := s.Store.DueForExpiry(ctx, now, batch); err != nil {
		s.Log.ErrorContext(ctx, "list expirable payments", "error", err)
	} else {
		for _, pid := range ids {
			_, err := s.Store.Mutate(ctx, pid, func(p *payment.Payment) ([]payment.Transition, error) {
				due := (p.Status == payment.StatusAuthorized && p.AuthorizationExpiresAt != nil && !p.AuthorizationExpiresAt.After(now)) ||
					(p.Status == payment.StatusPending && p.PendingExpiresAt != nil && !p.PendingExpiresAt.After(now))
				if !due {
					return nil, nil // captured or voided since the query
				}
				t, err := p.Expire()
				if err != nil {
					return nil, err
				}
				return []payment.Transition{t}, nil
			})
			switch {
			case errors.Is(err, store.ErrOperationInProgress):
				// A provider call is in flight. Its result decides; the
				// next pass expires the payment if it is still due.
			case err != nil:
				s.Log.WarnContext(ctx, "expire payment", "payment_id", pid, "error", err)
			}
		}
	}

	// These two compare with database timestamps, so they use the wall clock.
	wall := time.Now()
	if n, err := s.Store.FailStaleOperations(ctx, wall.Add(-2*time.Minute)); err != nil {
		s.Log.ErrorContext(ctx, "fail stale operations", "error", err)
	} else if n > 0 {
		s.Log.WarnContext(ctx, "failed stale operations", "count", n)
	}

	if _, err := s.Store.PurgeIdempotency(ctx, wall.Add(-24*time.Hour), wall.Add(-5*time.Minute)); err != nil {
		s.Log.ErrorContext(ctx, "purge idempotency keys", "error", err)
	}
}

// RunMaintenance calls MaintenanceOnce every interval until ctx ends.
func (s *Service) RunMaintenance(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.MaintenanceOnce(ctx)
		}
	}
}
