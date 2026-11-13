package payment

import (
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
)

// View is the public JSON form of a payment. The API responses and the
// webhook payloads use the same shape.
type View struct {
	ID                     string       `json:"id"`
	Object                 string       `json:"object"`
	Status                 Status       `json:"status"`
	Method                 Method       `json:"method"`
	Amount                 money.Amount `json:"amount"`
	Currency               string       `json:"currency"`
	AuthorizedAmount       money.Amount `json:"authorized_amount"`
	CapturedAmount         money.Amount `json:"captured_amount"`
	RefundedAmount         money.Amount `json:"refunded_amount"`
	Card                   *CardView    `json:"card,omitempty"`
	Pix                    *PixView     `json:"pix,omitempty"`
	AuthorizationExpiresAt *time.Time   `json:"authorization_expires_at,omitempty"`
	DeclineCode            string       `json:"decline_code,omitempty"`
	Description            string       `json:"description,omitempty"`
	Reference              string       `json:"reference,omitempty"`
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`
}

// CardView is the card part of View. Only safe fields.
type CardView struct {
	Brand string `json:"brand"`
	Last4 string `json:"last4"`
}

// PixView is the Pix part of View.
type PixView struct {
	CopyPaste string     `json:"copy_paste,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// View converts a payment to its public form.
func (p Payment) View() View {
	v := View{
		ID: p.ID, Object: "payment", Status: p.Status, Method: p.Method,
		Amount: p.Amount, Currency: p.Currency,
		AuthorizedAmount: p.AuthorizedAmount, CapturedAmount: p.CapturedAmount, RefundedAmount: p.RefundedAmount,
		AuthorizationExpiresAt: p.AuthorizationExpiresAt, DeclineCode: p.DeclineCode,
		Description: p.Description, Reference: p.Reference,
		CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
	if p.Method == MethodCard {
		v.Card = &CardView{Brand: p.CardBrand, Last4: p.CardLast4}
	}
	if p.Method == MethodPix {
		v.Pix = &PixView{CopyPaste: p.PixPayload, ExpiresAt: p.PendingExpiresAt}
	}
	return v
}
