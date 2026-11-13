package acquirer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
)

func TestSimulatorMagicAmounts(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		amount   money.Amount
		last4    string
		outcome  Outcome
		decline  string
		hasRef   bool
		timesOut bool
	}{
		{"approved", "card", 10000, "1111", Approved, "", true, false},
		{"insufficient funds", "card", 1051, "1111", Declined, "insufficient_funds", false, false},
		{"do not honor", "card", 1005, "1111", Declined, "do_not_honor", false, false},
		{"declined card", "card", 10000, "0002", Declined, "card_declined", false, false},
		{"delayed", "card", 1093, "1111", Pending, "", true, false},
		{"timeout", "card", 1091, "1111", "", "", false, true},
		{"pix pending", "pix", 1051, "", Pending, "", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSimulator(time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			res, err := s.Authorize(ctx, Request{IdempotencyKey: tt.name, Method: tt.method, Amount: tt.amount, CardLast4: tt.last4})
			if tt.timesOut {
				if !errors.Is(err, ErrTimeout) {
					t.Fatalf("err = %v, want timeout", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tt.outcome || res.DeclineCode != tt.decline || (res.ProviderRef != "") != tt.hasRef {
				t.Fatalf("res = %+v", res)
			}
		})
	}
}

func TestSimulatorIdempotentAuthorize(t *testing.T) {
	s := NewSimulator(time.Second)
	req := Request{IdempotencyKey: "op_1", Method: "card", Amount: 500, CardLast4: "1111"}
	a, _ := s.Authorize(context.Background(), req)
	b, _ := s.Authorize(context.Background(), req)
	if a.ProviderRef != b.ProviderRef {
		t.Fatal("same key must return the same reference")
	}
}

func TestSimulatorDelayedAndPix(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSimulator(5 * time.Second)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	res, _ := s.Authorize(ctx, Request{IdempotencyKey: "a", Method: "card", Amount: 1093})
	if st, _ := s.Status(ctx, res.ProviderRef); st.Outcome != Pending {
		t.Fatal("must stay pending before the delay")
	}
	now = now.Add(5 * time.Second)
	if st, _ := s.Status(ctx, res.ProviderRef); st.Outcome != Approved {
		t.Fatal("must be approved after the delay")
	}
	pix, _ := s.Authorize(ctx, Request{IdempotencyKey: "b", Method: "pix", Amount: 100})
	if st, _ := s.Status(ctx, pix.ProviderRef); st.Outcome != Pending {
		t.Fatal("unpaid pix must be pending")
	}
	if err := s.PayPix(pix.ProviderRef); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(ctx, pix.ProviderRef); st.Outcome != Approved {
		t.Fatal("paid pix must be approved")
	}
	if _, err := s.Status(ctx, "nope"); !errors.Is(err, ErrUnknownRef) {
		t.Fatal("unknown ref")
	}
	if err := s.PayPix(res.ProviderRef); !errors.Is(err, ErrUnknownRef) {
		t.Fatal("PayPix on a card ref must fail")
	}
}

func TestSimulatorMutationsAreIdempotent(t *testing.T) {
	s := NewSimulator(time.Second)
	ctx := context.Background()
	res, _ := s.Authorize(ctx, Request{IdempotencyKey: "a", Method: "card", Amount: 1000})
	for i := 0; i < 3; i++ {
		if err := s.Refund(ctx, res.ProviderRef, "r1", 100); err != nil {
			t.Fatal(err)
		}
	}
	if s.records[res.ProviderRef].refunded != 100 {
		t.Fatalf("refunded = %d, want 100", s.records[res.ProviderRef].refunded)
	}
	if err := s.Capture(ctx, "nope", "k", 1); !errors.Is(err, ErrUnknownRef) {
		t.Fatal("capture unknown ref")
	}
	if err := s.Void(ctx, res.ProviderRef, "v1"); err != nil || !s.records[res.ProviderRef].voided {
		t.Fatal("void")
	}
}

func newResilient(inner Acquirer, now *time.Time) *Resilient {
	return &Resilient{
		Inner:          inner,
		AttemptTimeout: 20 * time.Millisecond,
		TotalBudget:    time.Second,
		MaxAttempts:    3,
		Backoff:        resilience.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
		Breaker:        &resilience.Breaker{Threshold: 3, Cooldown: time.Minute, Now: func() time.Time { return *now }},
	}
}

func TestResilientRetriesTransientTimeout(t *testing.T) {
	now := time.Unix(0, 0)
	r := newResilient(NewSimulator(time.Second), &now)
	var outcomes []string
	r.Observe = func(_, o string, _ time.Duration) { outcomes = append(outcomes, o) }
	res, err := r.Authorize(context.Background(), Request{IdempotencyKey: "k", Method: "card", Amount: 1092})
	if err != nil || res.Outcome != Approved {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(outcomes) != 2 || outcomes[0] != "timeout" || outcomes[1] != "ok" {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestResilientOpensBreaker(t *testing.T) {
	now := time.Unix(0, 0)
	r := newResilient(NewSimulator(time.Second), &now)
	_, err := r.Authorize(context.Background(), Request{IdempotencyKey: "k", Method: "card", Amount: 1091})
	if !IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if r.Breaker.State() != resilience.Open {
		t.Fatal("3 timeouts must open the breaker")
	}
	// While open, calls fail fast without reaching the provider.
	start := time.Now()
	_, err = r.Authorize(context.Background(), Request{IdempotencyKey: "k2", Method: "card", Amount: 100})
	if !IsUnavailable(err) || time.Since(start) > 10*time.Millisecond {
		t.Fatalf("open breaker must fail fast: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := r.Authorize(context.Background(), Request{IdempotencyKey: "k3", Method: "card", Amount: 100}); err != nil {
		t.Fatalf("half-open trial must pass: %v", err)
	}
	if r.Breaker.State() != resilience.Closed {
		t.Fatal("breaker must close after a good trial")
	}
}

func TestResilientDoesNotRetryBusinessErrors(t *testing.T) {
	now := time.Unix(0, 0)
	r := newResilient(NewSimulator(time.Second), &now)
	calls := 0
	r.Observe = func(string, string, time.Duration) { calls++ }
	if err := r.Capture(context.Background(), "nope", "k", 1); !errors.Is(err, ErrUnknownRef) {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestResilientHonorsParentCancel(t *testing.T) {
	now := time.Unix(0, 0)
	r := newResilient(NewSimulator(time.Second), &now)
	r.AttemptTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	start := time.Now()
	_, _ = r.Authorize(ctx, Request{IdempotencyKey: "k", Method: "card", Amount: 1091})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("cancel must stop the call")
	}
}
