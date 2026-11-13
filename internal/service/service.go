// Package service holds the use cases of the gateway. It joins the
// domain (payment), the provider port (acquirer) and the store.
//
// Every provider call follows three phases so no database lock is held
// during network I/O:
//
//  1. lock the payment, check the rule, insert a pending operation, commit
//  2. call the provider with the operation id as idempotency key, on a
//     context detached from the client (see providerCtx)
//  3. lock the payment again, apply the domain change, record events,
//     ledger and outbox, mark the operation done, commit
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/acquirer"
	"github.com/kmadsdev/simple-payment-gateway/internal/card"
	"github.com/kmadsdev/simple-payment-gateway/internal/id"
	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/pix"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// MaxAmount is the largest single payment accepted: R$ 1.000.000,00.
const MaxAmount money.Amount = 100_000_000

// Config holds the business settings.
type Config struct {
	AuthorizationTTL time.Duration // how long an uncaptured authorization lives
	PendingTTL       time.Duration // Pix charge expiry and delayed-approval timeout
	PixMerchantName  string
	PixCity          string
}

// Service implements the use cases.
type Service struct {
	Store *store.Store
	Acq   acquirer.Acquirer
	Sim   *acquirer.Simulator // nil when the simulator endpoints are off
	Cfg   Config
	Now   func() time.Time
	Log   *slog.Logger
}

// Errors of the service layer.
var (
	ErrProviderUnavailable = errors.New("payment provider unavailable, try again later")
	ErrSimulatorDisabled   = errors.New("simulator is disabled")
)

// ValidationError is a bad input field. Msg never repeats the input value.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

func invalid(field, msg string) error { return &ValidationError{Field: field, Msg: msg} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// finishCtx keeps phase 3 alive when the client goes away: once the
// provider answered, the result must be recorded.
func finishCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// providerCallLimit bounds phase 2 when the client is gone. The acquirer
// decorator has its own, shorter budget; this is only a backstop.
const providerCallLimit = 30 * time.Second

// providerCtx detaches phase 2 from the client. Once a money-moving call is
// sent, a client hang-up must not abort it: the provider may already have
// done the work, and an aborted call turns a known result into an unknown
// one. Request values (request id) are kept.
func providerCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), providerCallLimit)
}

// ---- cards ----

// CardInput is the raw card data of a tokenization request.
type CardInput struct {
	Number   string
	CVV      string
	ExpMonth int
	ExpYear  int
}

// Tokenize validates a test card and keeps only the token, brand, last
// four digits and expiry. The number and the CVV are dropped here.
func (s *Service) Tokenize(ctx context.Context, merchantID string, in CardInput) (store.CardToken, error) {
	pan, err := card.Normalize(in.Number)
	if err != nil || !card.Luhn(pan) {
		return store.CardToken{}, invalid("number", card.ErrInvalidNumber.Error())
	}
	brand := card.GuessBrand(pan)
	if !card.ValidCVV(in.CVV, brand) {
		return store.CardToken{}, invalid("cvv", card.ErrInvalidCVV.Error())
	}
	if err := card.ValidateExpiry(in.ExpMonth, in.ExpYear, s.now()); err != nil {
		return store.CardToken{}, invalid("exp_month", err.Error())
	}
	t := store.CardToken{
		Token: id.New("tok"), MerchantID: merchantID, Brand: brand, Last4: card.Last4(pan),
		ExpMonth: in.ExpMonth, ExpYear: in.ExpYear,
	}
	if err := s.Store.CreateCardToken(ctx, &t); err != nil {
		return store.CardToken{}, err
	}
	return t, nil
}

// ---- payments ----

// CreateInput is a new payment intent.
type CreateInput struct {
	Amount      money.Amount
	Currency    string
	Method      string
	CardToken   string
	Description string
	Reference   string
}

// CreatePayment validates and stores a payment in status created.
func (s *Service) CreatePayment(ctx context.Context, merchantID string, in CreateInput) (payment.Payment, error) {
	switch {
	case !in.Amount.Positive():
		return payment.Payment{}, invalid("amount", "must be a positive integer in minor units (centavos)")
	case in.Amount > MaxAmount:
		return payment.Payment{}, invalid("amount", "is above the maximum of 100000000")
	case !money.ValidCurrency(in.Currency):
		return payment.Payment{}, invalid("currency", "only BRL is supported")
	case len(in.Description) > 255 || len(in.Reference) > 64:
		return payment.Payment{}, invalid("description", "description max 255, reference max 64 characters")
	}
	p := payment.Payment{
		ID: id.New("pay"), MerchantID: merchantID, Amount: in.Amount, Currency: in.Currency,
		Status: payment.StatusCreated, Description: in.Description, Reference: in.Reference,
	}
	switch payment.Method(in.Method) {
	case payment.MethodCard:
		if in.CardToken == "" {
			return payment.Payment{}, invalid("card_token", "is required for method card")
		}
		t, err := s.Store.GetCardToken(ctx, merchantID, in.CardToken)
		if errors.Is(err, store.ErrNotFound) {
			return payment.Payment{}, invalid("card_token", "not found")
		}
		if err != nil {
			return payment.Payment{}, err
		}
		p.Method, p.CardToken, p.CardBrand, p.CardLast4 = payment.MethodCard, t.Token, t.Brand, t.Last4
	case payment.MethodPix:
		if in.CardToken != "" {
			return payment.Payment{}, invalid("card_token", "is not allowed for method pix")
		}
		p.Method = payment.MethodPix
	default:
		return payment.Payment{}, invalid("method", "must be card or pix")
	}
	if err := s.Store.CreatePayment(ctx, &p); err != nil {
		return payment.Payment{}, err
	}
	return p, nil
}

// Authorize asks the provider to approve the payment. Card: approved,
// declined or pending (delayed approval). Pix: pending with a payload.
func (s *Service) Authorize(ctx context.Context, merchantID, paymentID string) (payment.Payment, error) {
	p, opID, _, err := s.Store.BeginOperation(ctx, merchantID, paymentID, "authorize",
		func(p payment.Payment) (money.Amount, error) {
			if p.Status != payment.StatusCreated {
				return 0, &payment.TransitionError{Op: "authorize", From: p.Status}
			}
			return p.Amount, nil
		})
	if err != nil {
		return payment.Payment{}, err
	}
	pctx, pcancel := providerCtx(ctx)
	res, callErr := s.Acq.Authorize(pctx, acquirer.Request{
		IdempotencyKey: opID, PaymentID: p.ID, Method: string(p.Method), Amount: p.Amount,
		Currency: p.Currency, CardToken: p.CardToken, CardLast4: p.CardLast4,
	})
	pcancel()
	fctx, cancel := finishCtx(ctx)
	defer cancel()
	if callErr != nil {
		return s.providerFailed(fctx, opID, "authorize", callErr)
	}
	now := s.now()
	return s.Store.FinishOperation(fctx, opID, "", func(p *payment.Payment) ([]payment.Transition, error) {
		var t payment.Transition
		var err error
		switch res.Outcome {
		case acquirer.Approved:
			t, err = p.Authorize(res.ProviderRef, now.Add(s.Cfg.AuthorizationTTL))
		case acquirer.Declined:
			t, err = p.Decline(res.DeclineCode)
		case acquirer.Pending:
			t, err = p.MarkPending(res.ProviderRef, now.Add(s.Cfg.PendingTTL))
			if err == nil && p.Method == payment.MethodPix {
				p.PixPayload, err = pix.Build(pix.Payload{
					TxID: strings.ReplaceAll(p.ID, "_", ""), Amount: p.Amount,
					MerchantName: s.Cfg.PixMerchantName, City: s.Cfg.PixCity,
				})
			}
		default:
			err = fmt.Errorf("unknown provider outcome %q", res.Outcome)
		}
		if err != nil {
			return nil, err
		}
		return []payment.Transition{t}, nil
	})
}

func (s *Service) providerFailed(ctx context.Context, opID, op string, callErr error) (payment.Payment, error) {
	code := "provider_error"
	if acquirer.IsUnavailable(callErr) {
		code = "provider_unavailable"
	}
	if _, err := s.Store.FinishOperation(ctx, opID, code, nil); err != nil {
		return payment.Payment{}, errors.Join(callErr, err)
	}
	s.Log.WarnContext(ctx, "provider call failed", "op", op, "operation_id", opID, "error", callErr.Error())
	if code == "provider_unavailable" {
		return payment.Payment{}, ErrProviderUnavailable
	}
	return payment.Payment{}, callErr
}

// runOp is the three-phase flow for capture, void and refund.
func (s *Service) runOp(ctx context.Context, merchantID, paymentID, kind string,
	prepare func(p payment.Payment) (money.Amount, error),
	call func(ctx context.Context, p payment.Payment, opID string, amount money.Amount) error,
	mutate func(p *payment.Payment, amount money.Amount) (payment.Transition, error),
) (payment.Payment, error) {
	p, opID, amount, err := s.Store.BeginOperation(ctx, merchantID, paymentID, kind, prepare)
	if err != nil {
		return payment.Payment{}, err
	}
	pctx, pcancel := providerCtx(ctx)
	callErr := call(pctx, p, opID, amount)
	pcancel()
	fctx, cancel := finishCtx(ctx)
	defer cancel()
	if callErr != nil {
		return s.providerFailed(fctx, opID, kind, callErr)
	}
	return s.Store.FinishOperation(fctx, opID, "", func(p *payment.Payment) ([]payment.Transition, error) {
		t, err := mutate(p, amount)
		if err != nil {
			return nil, err
		}
		return []payment.Transition{t}, nil
	})
}

// Capture captures amount, or the full authorized amount when nil.
func (s *Service) Capture(ctx context.Context, merchantID, paymentID string, amount *money.Amount) (payment.Payment, error) {
	return s.runOp(ctx, merchantID, paymentID, "capture",
		func(p payment.Payment) (money.Amount, error) {
			a := p.AuthorizedAmount
			if amount != nil {
				a = *amount
			}
			_, err := p.Capture(a) // dry run on a copy
			return a, err
		},
		func(ctx context.Context, p payment.Payment, opID string, a money.Amount) error {
			return s.Acq.Capture(ctx, p.ProviderRef, opID, a)
		},
		func(p *payment.Payment, a money.Amount) (payment.Transition, error) { return p.Capture(a) })
}

// Void cancels an authorization before capture.
func (s *Service) Void(ctx context.Context, merchantID, paymentID string) (payment.Payment, error) {
	return s.runOp(ctx, merchantID, paymentID, "void",
		func(p payment.Payment) (money.Amount, error) {
			_, err := p.Void()
			return p.AuthorizedAmount, err
		},
		func(ctx context.Context, p payment.Payment, opID string, _ money.Amount) error {
			return s.Acq.Void(ctx, p.ProviderRef, opID)
		},
		func(p *payment.Payment, _ money.Amount) (payment.Transition, error) { return p.Void() })
}

// Refund refunds amount, or everything still refundable when nil.
func (s *Service) Refund(ctx context.Context, merchantID, paymentID string, amount *money.Amount) (payment.Payment, error) {
	return s.runOp(ctx, merchantID, paymentID, "refund",
		func(p payment.Payment) (money.Amount, error) {
			a := p.Refundable()
			if amount != nil {
				a = *amount
			}
			_, err := p.Refund(a)
			return a, err
		},
		func(ctx context.Context, p payment.Payment, opID string, a money.Amount) error {
			return s.Acq.Refund(ctx, p.ProviderRef, opID, a)
		},
		func(p *payment.Payment, a money.Amount) (payment.Transition, error) { return p.Refund(a) })
}

// Get returns one payment of the merchant.
func (s *Service) Get(ctx context.Context, merchantID, paymentID string) (payment.Payment, error) {
	return s.Store.GetPayment(ctx, merchantID, paymentID)
}

// ---- provider updates and simulator ----

// SyncWithProvider asks the provider about a pending payment and applies
// the answer (delayed card approval, paid Pix).
func (s *Service) SyncWithProvider(ctx context.Context, paymentID string) (payment.Payment, error) {
	p, err := s.Store.PaymentByID(ctx, paymentID)
	if err != nil {
		return p, err
	}
	if p.Status != payment.StatusPending || p.ProviderRef == "" {
		return p, nil
	}
	res, err := s.Acq.Status(ctx, p.ProviderRef)
	if err != nil {
		return p, err
	}
	if res.Outcome == acquirer.Pending {
		return p, nil
	}
	now := s.now()
	return s.Store.Mutate(ctx, paymentID, func(p *payment.Payment) ([]payment.Transition, error) {
		if p.Status != payment.StatusPending {
			return nil, nil // someone else got here first
		}
		var t payment.Transition
		var err error
		switch {
		case res.Outcome == acquirer.Declined:
			t, err = p.Decline(res.DeclineCode)
		case p.Method == payment.MethodPix:
			t, err = p.ConfirmPix()
		default:
			t, err = p.Authorize(p.ProviderRef, now.Add(s.Cfg.AuthorizationTTL))
		}
		if err != nil {
			return nil, err
		}
		return []payment.Transition{t}, nil
	})
}

// SimulatePixPaid plays the payer: it reads the copy-and-paste payload
// like a bank app would, marks the charge paid at the simulator, and syncs.
func (s *Service) SimulatePixPaid(ctx context.Context, merchantID, paymentID string) (payment.Payment, error) {
	if s.Sim == nil {
		return payment.Payment{}, ErrSimulatorDisabled
	}
	p, err := s.Store.GetPayment(ctx, merchantID, paymentID)
	if err != nil {
		return p, err
	}
	if p.Method != payment.MethodPix {
		return p, payment.ErrWrongMethod
	}
	if p.Status != payment.StatusPending {
		return p, &payment.TransitionError{Op: "pix_confirm", From: p.Status}
	}
	decoded, err := pix.Parse(p.PixPayload)
	if err != nil {
		return p, err
	}
	if decoded.Amount != p.Amount {
		return p, errors.New("pix payload amount does not match the payment")
	}
	if err := s.Sim.PayPix(p.ProviderRef); err != nil {
		return p, err
	}
	return s.SyncWithProvider(ctx, paymentID)
}
