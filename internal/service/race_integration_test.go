package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/acquirer"
	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// gatedAcquirer holds every Capture until release is closed, so a test can
// act while a provider call is in flight.
type gatedAcquirer struct {
	acquirer.Acquirer
	entered   chan struct{}
	release   chan struct{}
	sawCancel atomic.Bool
}

func newGated(inner acquirer.Acquirer) *gatedAcquirer {
	return &gatedAcquirer{Acquirer: inner, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gatedAcquirer) Capture(ctx context.Context, ref, key string, a money.Amount) error {
	g.entered <- struct{}{}
	<-g.release
	if err := ctx.Err(); err != nil {
		g.sawCancel.Store(true)
		return err
	}
	return g.Acquirer.Capture(ctx, ref, key, a)
}

func (e *env) authorizedCard(t *testing.T, amount money.Amount) payment.Payment {
	t.Helper()
	p := e.newCardPayment(t, amount)
	p, err := e.svc.Authorize(context.Background(), e.merchant, p.ID)
	if err != nil || p.Status != payment.StatusAuthorized {
		t.Fatalf("authorize: %s %v", p.Status, err)
	}
	return p
}

// TestExpiryDoesNotRaceAnInFlightCapture: the authorization TTL ends while
// the capture call is at the provider. The expiry job must not expire the
// payment under the capture; the capture must win and the ledger must hold.
func TestExpiryDoesNotRaceAnInFlightCapture(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.authorizedCard(t, 1000)
	g := newGated(e.svc.Acq)
	e.svc.Acq = g

	done := make(chan error, 1)
	go func() {
		_, err := e.svc.Capture(ctx, e.merchant, p.ID, nil)
		done <- err
	}()
	<-g.entered
	e.advance(8 * 24 * time.Hour) // the authorization is now past its TTL
	e.svc.MaintenanceOnce(ctx)
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("capture after a concurrent expiry pass: %v", err)
	}
	if got := e.checkLedger(t, p.ID); got.Status != payment.StatusCaptured {
		t.Fatalf("status %s, want captured", got.Status)
	}
	var pending int
	_ = e.svc.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM payment_operations WHERE payment_id = $1 AND status = 'pending'`,
		p.ID).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d operations left pending", pending)
	}
}

// TestConcurrentCaptureAndVoid races a capture and a void on the same
// authorization, many times. At most one of them may win.
func TestConcurrentCaptureAndVoid(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 15; round++ {
		p := e.authorizedCard(t, 1000)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		ops := []func() error{
			func() error { _, err := e.svc.Capture(context.Background(), e.merchant, p.ID, nil); return err },
			func() error { _, err := e.svc.Void(context.Background(), e.merchant, p.ID); return err },
		}
		for i, op := range ops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = op()
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, store.ErrOperationInProgress), errors.Is(err, payment.ErrIllegalTransition):
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		got := e.checkLedger(t, p.ID)
		if wins != 1 || (got.Status != payment.StatusCaptured && got.Status != payment.StatusVoided) {
			t.Fatalf("round %d: %d winners, status %s", round, wins, got.Status)
		}
	}
}

// TestClientHangUpDoesNotAbortProviderCall: the client goes away while the
// capture is at the provider. The call must still finish and be recorded,
// or the gateway would lose a capture the provider may have done.
func TestClientHangUpDoesNotAbortProviderCall(t *testing.T) {
	e := newEnv(t)
	p := e.authorizedCard(t, 1000)
	g := newGated(e.svc.Acq)
	e.svc.Acq = g

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.Capture(ctx, e.merchant, p.ID, nil)
		done <- err
	}()
	<-g.entered
	cancel() // client hung up
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("capture: %v", err)
	}
	if g.sawCancel.Load() {
		t.Fatal("the provider call saw the client's cancellation")
	}
	if got := e.checkLedger(t, p.ID); got.Status != payment.StatusCaptured {
		t.Fatalf("status %s, want captured", got.Status)
	}
}
