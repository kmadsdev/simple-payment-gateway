package acquirer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
)

// Magic values of the simulator. They are deterministic so tests and the
// curl walkthrough can trigger each path on purpose.
//
// By the cents part of the amount (amount % 100), card only:
//
//	51  declined, insufficient_funds   (R$ 10,51)
//	05  declined, do_not_honor         (R$ 10,05)
//	91  every attempt times out        (R$ 10,91) -> acquirer_unavailable
//	92  first attempt times out, retry is approved (R$ 10,92)
//	93  pending, approved after DelayedApproval (R$ 10,93)
//
// By card last four digits:
//
//	0002  declined, card_declined (test number 4000 0000 0000 0002)
const (
	CentsInsufficientFunds = 51
	CentsDoNotHonor        = 5
	CentsAlwaysTimeout     = 91
	CentsTimeoutOnce       = 92
	CentsDelayedApproval   = 93
	DeclinedLast4          = "0002"
)

type simRecord struct {
	method    string
	result    Result
	createdAt time.Time
	delayed   bool
	pixPaid   bool
	captured  money.Amount
	refunded  money.Amount
	voided    bool
}

// Simulator is an in-memory provider. State is lost on restart, which is
// fine for a study project and is documented in the README.
type Simulator struct {
	DelayedApproval time.Duration
	Now             func() time.Time

	mu       sync.Mutex
	records  map[string]*simRecord
	byKey    map[string]Result // idempotency: same key returns the same result
	attempts map[string]int
	applied  map[string]bool // capture/void/refund keys already applied
}

// NewSimulator returns a ready simulator.
func NewSimulator(delayedApproval time.Duration) *Simulator {
	return &Simulator{
		DelayedApproval: delayedApproval,
		records:         map[string]*simRecord{},
		byKey:           map[string]Result{},
		attempts:        map[string]int{},
		applied:         map[string]bool{},
	}
}

func (s *Simulator) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func newRef() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sim_" + hex.EncodeToString(b)
}

// hang blocks until ctx ends, like a provider that never answers.
func hang(ctx context.Context) error {
	<-ctx.Done()
	return fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
}

// Authorize applies the magic rules above.
func (s *Simulator) Authorize(ctx context.Context, req Request) (Result, error) {
	s.mu.Lock()
	if r, ok := s.byKey[req.IdempotencyKey]; ok {
		s.mu.Unlock()
		return r, nil
	}
	s.attempts[req.IdempotencyKey]++
	attempt := s.attempts[req.IdempotencyKey]
	s.mu.Unlock()

	cents := int64(req.Amount) % 100
	if req.Method == "card" {
		switch {
		case cents == CentsAlwaysTimeout:
			return Result{}, hang(ctx)
		case cents == CentsTimeoutOnce && attempt == 1:
			return Result{}, hang(ctx)
		}
	}
	rec := &simRecord{method: req.Method, createdAt: s.now()}
	res := Result{Outcome: Approved, ProviderRef: newRef()}
	switch {
	case req.Method == "pix":
		res.Outcome = Pending
	case req.CardLast4 == DeclinedLast4:
		res = Result{Outcome: Declined, DeclineCode: "card_declined"}
	case cents == CentsInsufficientFunds:
		res = Result{Outcome: Declined, DeclineCode: "insufficient_funds"}
	case cents == CentsDoNotHonor:
		res = Result{Outcome: Declined, DeclineCode: "do_not_honor"}
	case cents == CentsDelayedApproval:
		res.Outcome = Pending
		rec.delayed = true
	}
	rec.result = res
	s.mu.Lock()
	defer s.mu.Unlock()
	if res.ProviderRef != "" {
		s.records[res.ProviderRef] = rec
	}
	s.byKey[req.IdempotencyKey] = res
	return res, nil
}

// Status resolves delayed approvals and paid Pix charges.
func (s *Simulator) Status(ctx context.Context, ref string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[ref]
	if !ok {
		return Result{}, ErrUnknownRef
	}
	switch {
	case rec.delayed && s.now().Sub(rec.createdAt) >= s.DelayedApproval:
		rec.delayed = false
		rec.result.Outcome = Approved
	case rec.method == "pix" && rec.pixPaid:
		rec.result.Outcome = Approved
	}
	return rec.result, nil
}

// PayPix simulates the payer paying the Pix charge in a bank app.
func (s *Simulator) PayPix(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[ref]
	if !ok || rec.method != "pix" {
		return ErrUnknownRef
	}
	rec.pixPaid = true
	return nil
}

func (s *Simulator) mutate(ctx context.Context, ref, key string, fn func(*simRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[ref]
	if !ok {
		return ErrUnknownRef
	}
	if s.applied[key] {
		return nil // idempotent replay
	}
	if err := fn(rec); err != nil {
		return err
	}
	s.applied[key] = true
	return nil
}

// Capture records a capture.
func (s *Simulator) Capture(ctx context.Context, ref, key string, amount money.Amount) error {
	return s.mutate(ctx, ref, key, func(r *simRecord) error { r.captured += amount; return nil })
}

// Void records a void.
func (s *Simulator) Void(ctx context.Context, ref, key string) error {
	return s.mutate(ctx, ref, key, func(r *simRecord) error { r.voided = true; return nil })
}

// Refund records a refund.
func (s *Simulator) Refund(ctx context.Context, ref, key string, amount money.Amount) error {
	return s.mutate(ctx, ref, key, func(r *simRecord) error { r.refunded += amount; return nil })
}
