package payment

import "github.com/kmadsdev/simple-payment-gateway/internal/money"

// Account is a ledger account scoped to one payment.
//
//	customer  money that belongs to the payer
//	hold      authorized and not yet captured
//	merchant  captured for the store, net of refunds
//
// A debit is a positive amount, a credit is negative. Every journal sums
// to zero, so the sum of all entries of a payment is always zero.
type Account string

// Ledger accounts.
const (
	AccountCustomer Account = "customer"
	AccountHold     Account = "hold"
	AccountMerchant Account = "merchant"
)

// Entry is one ledger line.
type Entry struct {
	Account Account
	Amount  money.Amount
}

// Journal is a balanced group of entries written together.
type Journal struct {
	Kind    string // authorize, capture, release, refund
	Entries []Entry
}

// Sum returns the total of the entries. A balanced journal sums to zero.
func (j Journal) Sum() money.Amount {
	var s money.Amount
	for _, e := range j.Entries {
		s += e.Amount
	}
	return s
}

func journal(kind string, debit, credit Account, a money.Amount) Journal {
	return Journal{Kind: kind, Entries: []Entry{{debit, a}, {credit, -a}}}
}

func journalAuthorize(a money.Amount) Journal {
	return journal("authorize", AccountHold, AccountCustomer, a)
}

func journalCapture(a money.Amount) Journal {
	return journal("capture", AccountMerchant, AccountHold, a)
}

func journalRelease(a money.Amount) Journal {
	return journal("release", AccountCustomer, AccountHold, a)
}

func journalRefund(a money.Amount) Journal {
	return journal("refund", AccountCustomer, AccountMerchant, a)
}

// Balances adds up entries per account.
func Balances(entries []Entry) map[Account]money.Amount {
	out := map[Account]money.Amount{}
	for _, e := range entries {
		out[e.Account] += e.Amount
	}
	return out
}

// CheckInvariants compares a payment with its ledger entries. It returns
// a non-empty list of problems when they disagree:
//
//	sum(all entries)        == 0
//	balance(hold)           == authorized - captured - released, and the
//	                           hold is zero once the payment left "authorized"
//	balance(merchant)       == captured - refunded >= 0
//	balance(customer)       == -(hold + merchant)
//	0 <= refunded <= captured <= authorized <= amount
func CheckInvariants(p Payment, entries []Entry) []string {
	var probs []string
	b := Balances(entries)
	var total money.Amount
	for _, v := range b {
		total += v
	}
	if total != 0 {
		probs = append(probs, "ledger does not sum to zero")
	}
	if b[AccountMerchant] != p.CapturedAmount-p.RefundedAmount {
		probs = append(probs, "merchant balance != captured - refunded")
	}
	wantHold := money.Amount(0)
	if p.Status == StatusAuthorized {
		wantHold = p.AuthorizedAmount
	}
	if b[AccountHold] != wantHold {
		probs = append(probs, "hold balance does not match status")
	}
	if b[AccountCustomer] != -(b[AccountHold] + b[AccountMerchant]) {
		probs = append(probs, "customer balance mismatch")
	}
	ordered := 0 <= p.RefundedAmount && p.RefundedAmount <= p.CapturedAmount &&
		p.CapturedAmount <= p.AuthorizedAmount && p.AuthorizedAmount <= p.Amount
	if !ordered {
		probs = append(probs, "amount ordering broken")
	}
	return probs
}
