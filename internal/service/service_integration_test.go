package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/acquirer"
	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
	"github.com/kmadsdev/simple-payment-gateway/internal/service"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
	"github.com/kmadsdev/simple-payment-gateway/internal/testdb"
)

type env struct {
	svc      *service.Service
	sim      *acquirer.Simulator
	merchant string
	clock    *atomic.Int64 // unix nanoseconds
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	sim := acquirer.NewSimulator(5 * time.Second)
	sim.Now = now
	svc := &service.Service{
		Store: store.New(pool),
		Acq: &acquirer.Resilient{
			Inner: sim, AttemptTimeout: 50 * time.Millisecond, TotalBudget: time.Second, MaxAttempts: 3,
			Backoff: resilience.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
			Breaker: &resilience.Breaker{Threshold: 100, Cooldown: time.Second},
		},
		Sim: sim,
		Cfg: service.Config{AuthorizationTTL: 7 * 24 * time.Hour, PendingTTL: 30 * time.Minute,
			PixMerchantName: "Test Store", PixCity: "Sao Paulo"},
		Now: now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	nm, err := svc.CreateMerchant(context.Background(), "Test Store", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, sim: sim, merchant: nm.Merchant.ID, clock: clock}
}

func (e *env) advance(d time.Duration) { e.clock.Add(int64(d)) }

func (e *env) newCardPayment(t *testing.T, amount money.Amount) payment.Payment {
	t.Helper()
	ctx := context.Background()
	tok, err := e.svc.Tokenize(ctx, e.merchant, service.CardInput{Number: "4111 1111 1111 1111", CVV: "123", ExpMonth: 12, ExpYear: 2099})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.CreatePayment(ctx, e.merchant, service.CreateInput{Amount: amount, Currency: "BRL", Method: "card", CardToken: tok.Token})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// checkLedger loads the payment and its ledger and checks every invariant.
func (e *env) checkLedger(t *testing.T, paymentID string) payment.Payment {
	t.Helper()
	ctx := context.Background()
	p, err := e.svc.Get(ctx, e.merchant, paymentID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.svc.Store.Ledger(ctx, e.merchant, paymentID)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]payment.Entry, len(rows))
	for i, r := range rows {
		entries[i] = payment.Entry{Account: r.Account, Amount: r.Amount}
	}
	if probs := payment.CheckInvariants(p, entries); len(probs) > 0 {
		t.Fatalf("payment %s (%s): %v", p.ID, p.Status, probs)
	}
	return p
}

// TestLedgerInvariantAfterRandomOperations runs random operation
// sequences against PostgreSQL and checks the ledger after every step.
func TestLedgerInvariantAfterRandomOperations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 25; i++ {
		amount := money.Amount(100 + r.IntN(50000))
		if c := int64(amount) % 100; c == 91 || c == 92 || c == 93 || c == 51 || c == 5 {
			amount++ // keep the magic amounts out of this test
		}
		p := e.newCardPayment(t, amount)
		for step := 0; step < 6; step++ {
			a := money.Amount(1 + r.IntN(int(amount)+1))
			var err error
			switch r.IntN(6) {
			case 0, 1:
				_, err = e.svc.Authorize(ctx, e.merchant, p.ID)
			case 2:
				_, err = e.svc.Capture(ctx, e.merchant, p.ID, &a)
			case 3:
				_, err = e.svc.Refund(ctx, e.merchant, p.ID, &a)
			case 4:
				_, err = e.svc.Void(ctx, e.merchant, p.ID)
			case 5:
				e.advance(8 * 24 * time.Hour)
				e.svc.MaintenanceOnce(ctx)
			}
			if err != nil && !errors.Is(err, payment.ErrIllegalTransition) && !errors.Is(err, payment.ErrAmountTooHigh) {
				t.Fatalf("unexpected error: %v", err)
			}
			e.checkLedger(t, p.ID)
		}
	}
	// Database-wide check: every journal and every payment sums to zero.
	var bad int
	err := e.svc.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT journal_id FROM ledger_entries GROUP BY journal_id HAVING sum(amount) <> 0
		UNION ALL
		SELECT payment_id FROM ledger_entries GROUP BY payment_id HAVING sum(amount) <> 0) x`).Scan(&bad)
	if err != nil || bad != 0 {
		t.Fatalf("unbalanced groups: %d, err %v", bad, err)
	}
}

func TestConcurrentRefundsNeverExceedCaptured(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.newCardPayment(t, 1000)
	if _, err := e.svc.Authorize(ctx, e.merchant, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Capture(ctx, e.merchant, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := money.Amount(300)
			_, err := e.svc.Refund(ctx, e.merchant, p.ID, &a)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, store.ErrOperationInProgress), errors.Is(err, payment.ErrAmountTooHigh):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	got := e.checkLedger(t, p.ID)
	if got.RefundedAmount != money.Amount(ok.Load()*300) || got.RefundedAmount > 1000 || ok.Load() == 0 {
		t.Fatalf("refunded %d with %d successes", got.RefundedAmount, ok.Load())
	}
}

func TestDelayedApprovalThenExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.newCardPayment(t, 1093)
	p, err := e.svc.Authorize(ctx, e.merchant, p.ID)
	if err != nil || p.Status != payment.StatusPending {
		t.Fatalf("status %s err %v", p.Status, err)
	}
	e.svc.MaintenanceOnce(ctx)
	if p = e.checkLedger(t, p.ID); p.Status != payment.StatusPending {
		t.Fatal("must stay pending before the provider decides")
	}
	e.advance(6 * time.Second)
	e.svc.MaintenanceOnce(ctx)
	if p = e.checkLedger(t, p.ID); p.Status != payment.StatusAuthorized {
		t.Fatalf("status %s, want authorized", p.Status)
	}
	e.advance(8 * 24 * time.Hour)
	e.svc.MaintenanceOnce(ctx)
	if p = e.checkLedger(t, p.ID); p.Status != payment.StatusExpired {
		t.Fatalf("status %s, want expired", p.Status)
	}
}

func TestProviderTimeoutLeavesPaymentUnchanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.newCardPayment(t, 1091)
	if _, err := e.svc.Authorize(ctx, e.merchant, p.ID); !errors.Is(err, service.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want provider unavailable", err)
	}
	if got := e.checkLedger(t, p.ID); got.Status != payment.StatusCreated {
		t.Fatalf("status %s, want created", got.Status)
	}
	var failed int
	_ = e.svc.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM payment_operations WHERE payment_id = $1
		AND status = 'failed' AND failure_code = 'provider_unavailable'`, p.ID).Scan(&failed)
	if failed != 1 {
		t.Fatalf("failed operations = %d, want 1", failed)
	}
}

func TestTransientTimeoutIsRetried(t *testing.T) {
	e := newEnv(t)
	p := e.newCardPayment(t, 1092)
	got, err := e.svc.Authorize(context.Background(), e.merchant, p.ID)
	if err != nil || got.Status != payment.StatusAuthorized {
		t.Fatalf("status %s err %v", got.Status, err)
	}
}

func TestPixPaidAndExpired(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, paid := range []bool{true, false} {
		p, err := e.svc.CreatePayment(ctx, e.merchant, service.CreateInput{Amount: 2590, Currency: "BRL", Method: "pix"})
		if err != nil {
			t.Fatal(err)
		}
		if p, err = e.svc.Authorize(ctx, e.merchant, p.ID); err != nil || p.Status != payment.StatusPending || p.PixPayload == "" {
			t.Fatalf("authorize pix: %s %v", p.Status, err)
		}
		if paid {
			if p, err = e.svc.SimulatePixPaid(ctx, e.merchant, p.ID); err != nil || p.Status != payment.StatusCaptured {
				t.Fatalf("pix paid: %s %v", p.Status, err)
			}
		} else {
			e.advance(31 * time.Minute)
			e.svc.MaintenanceOnce(ctx)
			if p = e.checkLedger(t, p.ID); p.Status != payment.StatusExpired {
				t.Fatalf("status %s, want expired", p.Status)
			}
		}
		e.checkLedger(t, p.ID)
	}
}

func TestDatabaseGuards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.newCardPayment(t, 1000)
	if _, err := e.svc.Authorize(ctx, e.merchant, p.ID); err != nil {
		t.Fatal(err)
	}
	pool := e.svc.Store.Pool
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET amount = amount + 1`); err == nil {
		t.Fatal("ledger_entries must be append-only")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM payment_events`); err == nil {
		t.Fatal("payment_events must be append-only")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries (payment_id, journal_id, kind, account, amount)
		VALUES ($1, 'jnl_bad', 'capture', 'merchant', 5)`, p.ID); err == nil {
		t.Fatal("an unbalanced journal must fail at commit")
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET captured_amount = amount + 1 WHERE id = $1`, p.ID); err == nil {
		t.Fatal("captured above authorized must violate the check constraint")
	}
}

func TestAuthenticate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	nm, err := e.svc.CreateMerchant(ctx, "Other", "https://example.com/hook", "")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := e.svc.Authenticate(ctx, nm.APIKey); err != nil || m.ID != nm.Merchant.ID {
		t.Fatalf("valid key rejected: %v", err)
	}
	bad := nm.APIKey[:len(nm.APIKey)-1] + "0"
	if bad == nm.APIKey {
		bad = nm.APIKey[:len(nm.APIKey)-1] + "1"
	}
	for _, k := range []string{"", "sk_test_short", bad} {
		if _, err := e.svc.Authenticate(ctx, k); !errors.Is(err, service.ErrUnauthenticated) {
			t.Fatalf("key %q: err = %v", k, err)
		}
	}
	if _, err := e.svc.CreateMerchant(ctx, "X", "ftp://nope", ""); err == nil {
		t.Fatal("bad webhook url accepted")
	}
}
