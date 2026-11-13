package payment

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
)

func TestTransitionTable(t *testing.T) {
	allowed := map[[2]Status]bool{
		{StatusCreated, StatusPending}:                     true,
		{StatusCreated, StatusAuthorized}:                  true,
		{StatusCreated, StatusDeclined}:                    true,
		{StatusPending, StatusAuthorized}:                  true,
		{StatusPending, StatusCaptured}:                    true,
		{StatusPending, StatusDeclined}:                    true,
		{StatusPending, StatusExpired}:                     true,
		{StatusAuthorized, StatusCaptured}:                 true,
		{StatusAuthorized, StatusVoided}:                   true,
		{StatusAuthorized, StatusExpired}:                  true,
		{StatusCaptured, StatusPartiallyRefunded}:          true,
		{StatusCaptured, StatusRefunded}:                   true,
		{StatusPartiallyRefunded, StatusPartiallyRefunded}: true,
		{StatusPartiallyRefunded, StatusRefunded}:          true,
	}
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			want := allowed[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range []Status{StatusRefunded, StatusVoided, StatusExpired, StatusDeclined} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
}

func newCard(amount money.Amount) *Payment {
	return &Payment{ID: "pay_1", Amount: amount, Currency: money.CurrencyBRL, Method: MethodCard, Status: StatusCreated}
}

var exp = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func TestCaptureAndRefund(t *testing.T) {
	tests := []struct {
		name       string
		capture    money.Amount
		refunds    []money.Amount
		wantStatus Status
		wantErr    error
	}{
		{"full capture", 1000, nil, StatusCaptured, nil},
		{"partial capture releases rest", 600, nil, StatusCaptured, nil},
		{"capture above authorized", 1001, nil, StatusAuthorized, ErrAmountTooHigh},
		{"capture zero", 0, nil, StatusAuthorized, ErrInvalidAmount},
		{"partial refund", 1000, []money.Amount{300}, StatusPartiallyRefunded, nil},
		{"two refunds to full", 1000, []money.Amount{300, 700}, StatusRefunded, nil},
		{"refund above captured", 600, []money.Amount{601}, StatusCaptured, ErrAmountTooHigh},
		{"refund after full refund", 1000, []money.Amount{1000, 1}, StatusRefunded, ErrIllegalTransition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newCard(1000)
			if _, err := p.Authorize("ref", exp); err != nil {
				t.Fatal(err)
			}
			var err error
			if _, err = p.Capture(tt.capture); err == nil {
				for _, r := range tt.refunds {
					if _, err = p.Refund(r); err != nil {
						break
					}
				}
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if p.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s", p.Status, tt.wantStatus)
			}
		})
	}
}

func TestPartialCaptureJournals(t *testing.T) {
	p := newCard(1000)
	if _, err := p.Authorize("ref", exp); err != nil {
		t.Fatal(err)
	}
	tr, err := p.Capture(600)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Journals) != 2 || tr.Journals[0].Kind != "capture" || tr.Journals[1].Kind != "release" {
		t.Fatalf("journals = %+v", tr.Journals)
	}
	if tr.Journals[1].Entries[0].Amount != 400 {
		t.Fatalf("release amount = %d, want 400", tr.Journals[1].Entries[0].Amount)
	}
}

func TestIllegalOperations(t *testing.T) {
	tests := []struct {
		name string
		run  func(p *Payment) error
	}{
		{"capture before authorize", func(p *Payment) error { _, err := p.Capture(1); return err }},
		{"void before authorize", func(p *Payment) error { _, err := p.Void(); return err }},
		{"refund before capture", func(p *Payment) error { _, err := p.Refund(1); return err }},
		{"expire created", func(p *Payment) error { _, err := p.Expire(); return err }},
		{"void after capture", func(p *Payment) error {
			_, _ = p.Authorize("r", exp)
			_, _ = p.Capture(10)
			_, err := p.Void()
			return err
		}},
		{"capture twice", func(p *Payment) error {
			_, _ = p.Authorize("r", exp)
			_, _ = p.Capture(10)
			_, err := p.Capture(10)
			return err
		}},
		{"authorize declined", func(p *Payment) error {
			_, _ = p.Decline("do_not_honor")
			_, err := p.Authorize("r", exp)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(newCard(100)); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("err = %v, want ErrIllegalTransition", err)
			}
		})
	}
}

func TestMethodRules(t *testing.T) {
	pix := &Payment{ID: "p", Amount: 500, Method: MethodPix, Status: StatusCreated}
	if _, err := pix.Authorize("r", exp); !errors.Is(err, ErrWrongMethod) {
		t.Fatalf("pix authorize: %v", err)
	}
	if _, err := pix.MarkPending("r", exp); err != nil {
		t.Fatal(err)
	}
	tr, err := pix.ConfirmPix()
	if err != nil {
		t.Fatal(err)
	}
	if pix.Status != StatusCaptured || len(tr.Journals) != 2 {
		t.Fatalf("pix confirm: %s %d", pix.Status, len(tr.Journals))
	}
	if _, err := pix.Capture(1); !errors.Is(err, ErrWrongMethod) {
		t.Fatalf("pix capture: %v", err)
	}
	if _, err := newCard(1).ConfirmPix(); !errors.Is(err, ErrWrongMethod) {
		t.Fatal("card ConfirmPix must fail")
	}
}

func TestEventType(t *testing.T) {
	if (Transition{To: StatusCaptured}).EventType() != "payment.captured" {
		t.Fatal("event type")
	}
}

// TestRandomSequencesKeepInvariants applies random operations to random
// payments and checks the ledger invariants after every step. Illegal
// operations must fail without changing anything.
func TestRandomSequencesKeepInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 2000; i++ {
		method := MethodCard
		if r.IntN(3) == 0 {
			method = MethodPix
		}
		p := &Payment{ID: "p", Amount: money.Amount(1 + r.IntN(100000)), Method: method, Status: StatusCreated}
		var entries []Entry
		for step := 0; step < 8; step++ {
			before := *p
			amt := money.Amount(r.IntN(int(p.Amount) + 2))
			var tr Transition
			var err error
			switch r.IntN(8) {
			case 0:
				tr, err = p.Authorize("r", exp)
			case 1:
				tr, err = p.MarkPending("r", exp)
			case 2:
				tr, err = p.Capture(amt)
			case 3:
				tr, err = p.Refund(amt)
			case 4:
				tr, err = p.Void()
			case 5:
				tr, err = p.Expire()
			case 6:
				tr, err = p.ConfirmPix()
			case 7:
				tr, err = p.Decline("x")
			}
			if err != nil {
				if p.Status != before.Status || p.CapturedAmount != before.CapturedAmount || p.RefundedAmount != before.RefundedAmount {
					t.Fatalf("failed op changed payment: %v", err)
				}
				continue
			}
			for _, j := range tr.Journals {
				if j.Sum() != 0 {
					t.Fatalf("unbalanced journal %+v", j)
				}
				entries = append(entries, j.Entries...)
			}
			if probs := CheckInvariants(*p, entries); len(probs) > 0 {
				t.Fatalf("iteration %d step %d (%s): %v", i, step, tr.Op, probs)
			}
		}
	}
}
