// Package acquirer defines the port between the gateway and a payment
// provider (acquirer or PSP), a deterministic simulator adapter, and a
// decorator that adds timeouts, retries and a circuit breaker.
//
// No real provider is integrated. The simulator is the only adapter.
package acquirer

import (
	"context"
	"errors"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
)

// Outcome of an authorization or a status query.
type Outcome string

// Outcomes.
const (
	Approved Outcome = "approved"
	Declined Outcome = "declined"
	Pending  Outcome = "pending"
)

// Request describes one authorization.
type Request struct {
	// IdempotencyKey lets the provider de-duplicate retries of the same
	// call. The gateway uses the operation id.
	IdempotencyKey string
	PaymentID      string
	Method         string // card or pix
	Amount         money.Amount
	Currency       string
	CardToken      string
	CardLast4      string
}

// Result of an authorization or a status query.
type Result struct {
	Outcome     Outcome
	ProviderRef string
	DeclineCode string
}

// Errors. ErrTimeout and ErrUnavailable are worth a retry.
var (
	ErrTimeout     = errors.New("acquirer timeout")
	ErrUnavailable = errors.New("acquirer unavailable")
	ErrUnknownRef  = errors.New("unknown provider reference")
)

// Retryable reports whether a new attempt can succeed.
func Retryable(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, ErrUnavailable)
}

// Acquirer is the port. Every call must honor ctx cancellation.
type Acquirer interface {
	Authorize(ctx context.Context, req Request) (Result, error)
	Capture(ctx context.Context, providerRef, idempotencyKey string, amount money.Amount) error
	Void(ctx context.Context, providerRef, idempotencyKey string) error
	Refund(ctx context.Context, providerRef, idempotencyKey string, amount money.Amount) error
	// Status returns the current decision for a pending authorization or
	// a Pix charge.
	Status(ctx context.Context, providerRef string) (Result, error)
}
