// Package payment is the domain core: the payment state machine and the
// double-entry ledger journals that each transition produces.
//
// The package has no I/O. Every operation mutates a Payment value and
// returns a Transition. The store persists the new payment row, the
// transition (event), its ledger entries and the outbox message in one
// database transaction.
package payment

import (
	"errors"
	"fmt"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
)

// Status is the state of a payment.
type Status string

// Payment states. See docs/state-machine.md.
const (
	StatusCreated           Status = "created"
	StatusPending           Status = "pending"
	StatusAuthorized        Status = "authorized"
	StatusCaptured          Status = "captured"
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
	StatusVoided            Status = "voided"
	StatusExpired           Status = "expired"
	StatusDeclined          Status = "declined"
)

// Method is the payment method.
type Method string

// Supported payment methods.
const (
	MethodCard Method = "card"
	MethodPix  Method = "pix"
)

// transitions is the full state machine. Anything not listed is illegal.
var transitions = map[Status][]Status{
	StatusCreated:           {StatusPending, StatusAuthorized, StatusDeclined},
	StatusPending:           {StatusAuthorized, StatusCaptured, StatusDeclined, StatusExpired},
	StatusAuthorized:        {StatusCaptured, StatusVoided, StatusExpired},
	StatusCaptured:          {StatusPartiallyRefunded, StatusRefunded},
	StatusPartiallyRefunded: {StatusPartiallyRefunded, StatusRefunded},
}

// AllStatuses lists every state, in lifecycle order.
var AllStatuses = []Status{
	StatusCreated, StatusPending, StatusAuthorized, StatusCaptured, StatusPartiallyRefunded,
	StatusRefunded, StatusVoided, StatusExpired, StatusDeclined,
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no transition leaves the state.
func (s Status) Terminal() bool { return len(transitions[s]) == 0 }

// Domain errors. The HTTP layer maps them to problem types.
var (
	ErrIllegalTransition = errors.New("illegal state transition")
	ErrInvalidAmount     = errors.New("invalid amount")
	ErrAmountTooHigh     = errors.New("amount above the allowed limit")
	ErrWrongMethod       = errors.New("operation not supported for this payment method")
)

// TransitionError carries the details of a rejected move.
type TransitionError struct {
	Op   string
	From Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("%s is not allowed when payment is %s", e.Op, e.From)
}

// Unwrap lets errors.Is(err, ErrIllegalTransition) match.
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// Payment is the aggregate root.
type Payment struct {
	ID                     string
	MerchantID             string
	Amount                 money.Amount
	Currency               string
	Method                 Method
	Status                 Status
	AuthorizedAmount       money.Amount
	CapturedAmount         money.Amount
	RefundedAmount         money.Amount
	CardToken              string
	CardBrand              string
	CardLast4              string
	PixPayload             string
	PendingExpiresAt       *time.Time
	AuthorizationExpiresAt *time.Time
	ProviderRef            string
	DeclineCode            string
	Description            string
	Reference              string
	Version                int
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Transition is one recorded state change and its money movements.
type Transition struct {
	Op       string // authorize, capture, void, refund, expire, decline, pending, pix_confirm
	From, To Status
	Amount   money.Amount
	Journals []Journal
}

// EventType is the webhook event name of the transition.
func (t Transition) EventType() string { return "payment." + string(t.To) }

func (p *Payment) move(op string, to Status, amount money.Amount, journals ...Journal) (Transition, error) {
	if !CanTransition(p.Status, to) {
		return Transition{}, &TransitionError{Op: op, From: p.Status}
	}
	t := Transition{Op: op, From: p.Status, To: to, Amount: amount, Journals: journals}
	p.Status = to
	return t, nil
}

// MarkPending records that the provider has not decided yet (card delayed
// approval) or that a Pix charge waits for the payer.
// The payment expires if the provider has not decided by expiresAt.
func (p *Payment) MarkPending(providerRef string, expiresAt time.Time) (Transition, error) {
	t, err := p.move("pending", StatusPending, 0)
	if err == nil {
		p.ProviderRef = providerRef
		p.PendingExpiresAt = &expiresAt
	}
	return t, err
}

// Authorize records an approved authorization of the full amount and
// opens a hold in the ledger. The hold expires at expiresAt.
func (p *Payment) Authorize(providerRef string, expiresAt time.Time) (Transition, error) {
	if p.Method != MethodCard {
		return Transition{}, ErrWrongMethod
	}
	t, err := p.move("authorize", StatusAuthorized, p.Amount, journalAuthorize(p.Amount))
	if err != nil {
		return t, err
	}
	p.ProviderRef = providerRef
	p.AuthorizedAmount = p.Amount
	p.AuthorizationExpiresAt = &expiresAt
	return t, nil
}

// Decline records a refusal from the provider.
func (p *Payment) Decline(code string) (Transition, error) {
	t, err := p.move("decline", StatusDeclined, 0)
	if err == nil {
		p.DeclineCode = code
	}
	return t, err
}

// Capture takes amount (0 < amount <= authorized) from the hold. A capture
// below the authorized amount releases the rest of the hold: one capture
// per payment, like most card acquirers.
func (p *Payment) Capture(amount money.Amount) (Transition, error) {
	if p.Method != MethodCard {
		return Transition{}, ErrWrongMethod
	}
	if p.Status != StatusAuthorized {
		return Transition{}, &TransitionError{Op: "capture", From: p.Status}
	}
	if !amount.Positive() {
		return Transition{}, ErrInvalidAmount
	}
	if amount > p.AuthorizedAmount {
		return Transition{}, fmt.Errorf("%w: capture %d above authorized %d (minor units)", ErrAmountTooHigh, amount, p.AuthorizedAmount)
	}
	journals := []Journal{journalCapture(amount)}
	if rest := p.AuthorizedAmount - amount; rest > 0 {
		journals = append(journals, journalRelease(rest))
	}
	t, err := p.move("capture", StatusCaptured, amount, journals...)
	if err == nil {
		p.CapturedAmount = amount
	}
	return t, err
}

// ConfirmPix records a paid Pix charge. Pix has no separate capture step,
// so the ledger gets the authorize and the capture journals together.
func (p *Payment) ConfirmPix() (Transition, error) {
	if p.Method != MethodPix {
		return Transition{}, ErrWrongMethod
	}
	t, err := p.move("pix_confirm", StatusCaptured, p.Amount,
		journalAuthorize(p.Amount), journalCapture(p.Amount))
	if err == nil {
		p.AuthorizedAmount = p.Amount
		p.CapturedAmount = p.Amount
	}
	return t, err
}

// Void cancels an authorization before capture and releases the hold.
func (p *Payment) Void() (Transition, error) {
	if p.Status != StatusAuthorized {
		return Transition{}, &TransitionError{Op: "void", From: p.Status}
	}
	return p.move("void", StatusVoided, p.AuthorizedAmount, journalRelease(p.AuthorizedAmount))
}

// Expire ends an uncaptured authorization (releasing the hold) or an
// unpaid pending charge (no money moved, so no journal).
func (p *Payment) Expire() (Transition, error) {
	switch p.Status {
	case StatusAuthorized:
		return p.move("expire", StatusExpired, p.AuthorizedAmount, journalRelease(p.AuthorizedAmount))
	case StatusPending:
		return p.move("expire", StatusExpired, 0)
	default:
		return Transition{}, &TransitionError{Op: "expire", From: p.Status}
	}
}

// Refundable is what can still go back to the customer.
func (p *Payment) Refundable() money.Amount { return p.CapturedAmount - p.RefundedAmount }

// Refund returns amount (0 < amount <= captured - refunded) to the customer.
func (p *Payment) Refund(amount money.Amount) (Transition, error) {
	if p.Status != StatusCaptured && p.Status != StatusPartiallyRefunded {
		return Transition{}, &TransitionError{Op: "refund", From: p.Status}
	}
	if !amount.Positive() {
		return Transition{}, ErrInvalidAmount
	}
	if amount > p.Refundable() {
		return Transition{}, fmt.Errorf("%w: refund %d above refundable %d (minor units)", ErrAmountTooHigh, amount, p.Refundable())
	}
	to := StatusPartiallyRefunded
	if amount == p.Refundable() {
		to = StatusRefunded
	}
	t, err := p.move("refund", to, amount, journalRefund(amount))
	if err == nil {
		p.RefundedAmount += amount
	}
	return t, err
}
