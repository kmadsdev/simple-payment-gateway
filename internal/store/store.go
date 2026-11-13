// Package store is the PostgreSQL adapter. It uses plain pgx and
// hand-written SQL (see docs/adr/0002). Every state change of a payment
// happens in one transaction that locks the payment row.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kmadsdev/simple-payment-gateway/internal/id"
	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
)

// Errors returned by the store.
var (
	ErrNotFound            = errors.New("not found")
	ErrOperationInProgress = errors.New("another operation on this payment is in progress")
	ErrVersionConflict     = errors.New("payment was changed concurrently")
)

// Store wraps a connection pool.
type Store struct {
	Pool *pgxpool.Pool
	// OnTransition is called after a commit, once per transition. It feeds
	// the metrics. Optional.
	OnTransition func(payment.Transition)
}

// New returns a Store.
func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// Ping checks the database for the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, fn)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- merchants ----

// Merchant is a store that uses the gateway.
type Merchant struct {
	ID            string
	Name          string
	APIKeyPrefix  string
	APIKeyHash    []byte
	WebhookURL    string
	WebhookSecret string
	CreatedAt     time.Time
}

// CreateMerchant inserts a merchant.
func (s *Store) CreateMerchant(ctx context.Context, m Merchant) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO merchants (id, name, api_key_prefix, api_key_hash, webhook_url, webhook_secret)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)`,
		m.ID, m.Name, m.APIKeyPrefix, m.APIKeyHash, m.WebhookURL, m.WebhookSecret)
	return err
}

// MerchantByKeyPrefix finds the merchant that owns an API key prefix.
func (s *Store) MerchantByKeyPrefix(ctx context.Context, prefix string) (Merchant, error) {
	var m Merchant
	err := s.Pool.QueryRow(ctx, `SELECT id, name, api_key_prefix, api_key_hash, coalesce(webhook_url, ''), webhook_secret, created_at
		FROM merchants WHERE api_key_prefix = $1`, prefix).
		Scan(&m.ID, &m.Name, &m.APIKeyPrefix, &m.APIKeyHash, &m.WebhookURL, &m.WebhookSecret, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// ---- card tokens ----

// CardToken is what is kept of a card. No PAN, no CVV.
type CardToken struct {
	Token      string
	MerchantID string
	Brand      string
	Last4      string
	ExpMonth   int
	ExpYear    int
	CreatedAt  time.Time
}

// CreateCardToken inserts a token.
func (s *Store) CreateCardToken(ctx context.Context, c *CardToken) error {
	return s.Pool.QueryRow(ctx, `INSERT INTO card_tokens (token, merchant_id, brand, last4, exp_month, exp_year)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		c.Token, c.MerchantID, c.Brand, c.Last4, c.ExpMonth, c.ExpYear).Scan(&c.CreatedAt)
}

// GetCardToken loads a token of the merchant.
func (s *Store) GetCardToken(ctx context.Context, merchantID, token string) (CardToken, error) {
	var c CardToken
	err := s.Pool.QueryRow(ctx, `SELECT token, merchant_id, brand, last4, exp_month, exp_year, created_at
		FROM card_tokens WHERE token = $1 AND merchant_id = $2`, token, merchantID).
		Scan(&c.Token, &c.MerchantID, &c.Brand, &c.Last4, &c.ExpMonth, &c.ExpYear, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ---- payments ----

const paymentCols = `id, merchant_id, amount, currency, method, status, authorized_amount, captured_amount,
	refunded_amount, coalesce(card_token, ''), coalesce(card_brand, ''), coalesce(card_last4, ''),
	coalesce(pix_payload, ''), pending_expires_at, authorization_expires_at, coalesce(provider_ref, ''),
	coalesce(decline_code, ''), coalesce(description, ''), coalesce(reference, ''), version, created_at, updated_at`

func scanPayment(row pgx.Row) (payment.Payment, error) {
	var p payment.Payment
	err := row.Scan(&p.ID, &p.MerchantID, &p.Amount, &p.Currency, &p.Method, &p.Status, &p.AuthorizedAmount,
		&p.CapturedAmount, &p.RefundedAmount, &p.CardToken, &p.CardBrand, &p.CardLast4, &p.PixPayload,
		&p.PendingExpiresAt, &p.AuthorizationExpiresAt, &p.ProviderRef, &p.DeclineCode, &p.Description,
		&p.Reference, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// CreatePayment inserts a new payment in status created and records the
// first event.
func (s *Store) CreatePayment(ctx context.Context, p *payment.Payment) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO payments (id, merchant_id, amount, currency, method, status, card_token,
			card_brand, card_last4, description, reference)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''))
			RETURNING version, created_at, updated_at`,
			p.ID, p.MerchantID, p.Amount, p.Currency, p.Method, p.Status, p.CardToken, p.CardBrand, p.CardLast4,
			p.Description, p.Reference).Scan(&p.Version, &p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO payment_events (payment_id, op, from_status, to_status, amount)
			VALUES ($1, 'create', NULL, $2, $3)`, p.ID, p.Status, p.Amount)
		return err
	})
}

// GetPayment loads a payment of the merchant.
func (s *Store) GetPayment(ctx context.Context, merchantID, paymentID string) (payment.Payment, error) {
	return scanPayment(s.Pool.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id = $1 AND merchant_id = $2`,
		paymentID, merchantID))
}

// PaymentByID loads a payment without a merchant scope. Internal use only
// (background workers).
func (s *Store) PaymentByID(ctx context.Context, paymentID string) (payment.Payment, error) {
	return scanPayment(s.Pool.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id = $1`, paymentID))
}

// Cursor is a position in the payment list (newest first).
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ListPayments returns up to limit payments older than the cursor.
func (s *Store) ListPayments(ctx context.Context, merchantID string, after *Cursor, limit int) ([]payment.Payment, error) {
	var rows pgx.Rows
	var err error
	if after == nil {
		rows, err = s.Pool.Query(ctx, `SELECT `+paymentCols+` FROM payments WHERE merchant_id = $1
			ORDER BY created_at DESC, id DESC LIMIT $2`, merchantID, limit)
	} else {
		rows, err = s.Pool.Query(ctx, `SELECT `+paymentCols+` FROM payments WHERE merchant_id = $1
			AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`,
			merchantID, after.CreatedAt, after.ID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []payment.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Event is one row of the payment history.
type Event struct {
	ID        int64        `json:"id"`
	Op        string       `json:"op"`
	From      string       `json:"from_status,omitempty"`
	To        string       `json:"to_status"`
	Amount    money.Amount `json:"amount"`
	CreatedAt time.Time    `json:"created_at"`
}

// Events returns the history of a payment, oldest first.
func (s *Store) Events(ctx context.Context, merchantID, paymentID string) ([]Event, error) {
	rows, err := s.Pool.Query(ctx, `SELECT e.id, e.op, coalesce(e.from_status, ''), e.to_status, e.amount, e.created_at
		FROM payment_events e JOIN payments p ON p.id = e.payment_id
		WHERE e.payment_id = $1 AND p.merchant_id = $2 ORDER BY e.id`, paymentID, merchantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.Op, &e.From, &e.To, &e.Amount, &e.CreatedAt)
		return e, err
	})
}

// LedgerEntry is one ledger line.
type LedgerEntry struct {
	ID        int64           `json:"id"`
	JournalID string          `json:"journal_id"`
	Kind      string          `json:"kind"`
	Account   payment.Account `json:"account"`
	Amount    money.Amount    `json:"amount"`
	CreatedAt time.Time       `json:"created_at"`
}

// Ledger returns the ledger lines of a payment, oldest first.
func (s *Store) Ledger(ctx context.Context, merchantID, paymentID string) ([]LedgerEntry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT l.id, l.journal_id, l.kind, l.account, l.amount, l.created_at
		FROM ledger_entries l JOIN payments p ON p.id = l.payment_id
		WHERE l.payment_id = $1 AND p.merchant_id = $2 ORDER BY l.id`, paymentID, merchantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LedgerEntry, error) {
		var e LedgerEntry
		err := r.Scan(&e.ID, &e.JournalID, &e.Kind, &e.Account, &e.Amount, &e.CreatedAt)
		return e, err
	})
}

// ---- state changes ----

// MutateFunc changes a locked payment through the domain and returns the
// transitions to record. Returning an error rolls everything back.
type MutateFunc func(p *payment.Payment) ([]payment.Transition, error)

func lockPayment(ctx context.Context, tx pgx.Tx, paymentID string) (payment.Payment, error) {
	return scanPayment(tx.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id = $1 FOR UPDATE`, paymentID))
}

// apply writes the new payment row, the events, the ledger journals and
// the outbox messages. The UPDATE checks the version as a second guard
// on top of the row lock.
func apply(ctx context.Context, tx pgx.Tx, p *payment.Payment, trs []payment.Transition) error {
	if len(trs) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE payments SET status = $1, authorized_amount = $2, captured_amount = $3,
		refunded_amount = $4, pix_payload = NULLIF($5, ''), pending_expires_at = $6, authorization_expires_at = $7,
		provider_ref = NULLIF($8, ''), decline_code = NULLIF($9, ''), version = version + 1, updated_at = now()
		WHERE id = $10 AND version = $11`,
		p.Status, p.AuthorizedAmount, p.CapturedAmount, p.RefundedAmount, p.PixPayload, p.PendingExpiresAt,
		p.AuthorizationExpiresAt, p.ProviderRef, p.DeclineCode, p.ID, p.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrVersionConflict
	}
	if err := tx.QueryRow(ctx, `SELECT version, updated_at FROM payments WHERE id = $1`, p.ID).
		Scan(&p.Version, &p.UpdatedAt); err != nil {
		return err
	}
	for _, t := range trs {
		if _, err := tx.Exec(ctx, `INSERT INTO payment_events (payment_id, op, from_status, to_status, amount)
			VALUES ($1, $2, $3, $4, $5)`, p.ID, t.Op, t.From, t.To, t.Amount); err != nil {
			return err
		}
		for _, j := range t.Journals {
			jid := id.New("jnl")
			for _, e := range j.Entries {
				if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (payment_id, journal_id, kind, account, amount)
					VALUES ($1, $2, $3, $4, $5)`, p.ID, jid, j.Kind, e.Account, e.Amount); err != nil {
					return err
				}
			}
		}
		evtID := id.New("evt")
		payload, err := json.Marshal(map[string]any{
			"id": evtID, "type": t.EventType(), "created_at": p.UpdatedAt.UTC(), "data": p.View(),
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (id, merchant_id, payment_id, event_type, payload)
			VALUES ($1, $2, $3, $4, $5)`, evtID, p.MerchantID, p.ID, t.EventType(), payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) notify(trs []payment.Transition) {
	if s.OnTransition == nil {
		return
	}
	for _, t := range trs {
		s.OnTransition(t)
	}
}

// Mutate locks the payment, runs fn and records the result. It is used by
// background changes (provider sync, expiry). It returns
// ErrOperationInProgress when a provider call on the payment is in flight:
// changing the payment under that call (for example expiring an
// authorization while its capture is at the provider) would lose the
// call's result. BeginOperation takes the same row lock before it inserts
// the operation, so the check below cannot miss one.
func (s *Store) Mutate(ctx context.Context, paymentID string, fn MutateFunc) (payment.Payment, error) {
	var out payment.Payment
	var trs []payment.Transition
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPayment(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_operations
			WHERE payment_id = $1 AND status = 'pending')`, paymentID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrOperationInProgress
		}
		if trs, err = fn(&p); err != nil {
			return err
		}
		if err := apply(ctx, tx, &p, trs); err != nil {
			return err
		}
		out = p
		return nil
	})
	if err == nil {
		s.notify(trs)
	}
	return out, err
}

// BeginOperation is phase 1 of a provider call. It locks the payment,
// runs check (a dry run of the domain rule), and inserts a pending
// operation row. The partial unique index rejects a second in-flight
// operation on the same payment. No lock is held during the provider call.
// check returns the amount of the operation (it may depend on the payment,
// for example "refund the rest").
func (s *Store) BeginOperation(ctx context.Context, merchantID, paymentID, kind string,
	check func(p payment.Payment) (money.Amount, error)) (payment.Payment, string, money.Amount, error) {
	var p payment.Payment
	var amount money.Amount
	opID := id.New("op")
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = lockPayment(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		if p.MerchantID != merchantID {
			return ErrNotFound
		}
		if amount, err = check(p); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO payment_operations (id, payment_id, kind, amount, status)
			VALUES ($1, $2, $3, $4, 'pending')`, opID, paymentID, kind, amount)
		if isUniqueViolation(err) {
			return ErrOperationInProgress
		}
		return err
	})
	return p, opID, amount, err
}

// FinishOperation is phase 3. It marks the operation succeeded or failed
// and, when fn is not nil, applies the domain change in the same
// transaction.
func (s *Store) FinishOperation(ctx context.Context, opID string, failureCode string, fn MutateFunc) (payment.Payment, error) {
	var out payment.Payment
	var trs []payment.Transition
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var paymentID string
		if err := tx.QueryRow(ctx, `SELECT payment_id FROM payment_operations WHERE id = $1 AND status = 'pending'`,
			opID).Scan(&paymentID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		p, err := lockPayment(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		status := "succeeded"
		if failureCode != "" {
			status = "failed"
		}
		if fn != nil {
			if trs, err = fn(&p); err != nil {
				return err
			}
			if err := apply(ctx, tx, &p, trs); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_operations SET status = $1, failure_code = NULLIF($2, ''),
			updated_at = now() WHERE id = $3`, status, failureCode, opID); err != nil {
			return err
		}
		out = p
		return nil
	})
	if err == nil {
		s.notify(trs)
	}
	return out, err
}

// ---- background work queries ----

// DueForExpiry returns payments whose authorization or pending window ended.
func (s *Store) DueForExpiry(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM payments
		WHERE (status = 'authorized' AND authorization_expires_at <= $1)
		   OR (status = 'pending' AND pending_expires_at <= $1)
		ORDER BY updated_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PendingAtProvider returns pending payments to poll at the provider.
func (s *Store) PendingAtProvider(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM payments WHERE status = 'pending' AND provider_ref IS NOT NULL
		ORDER BY updated_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// FailStaleOperations marks operations stuck in pending (for example after
// a crash between phase 1 and phase 3) as failed, so the payment is not
// blocked forever. It returns how many rows it changed.
func (s *Store) FailStaleOperations(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE payment_operations SET status = 'failed', failure_code = 'abandoned',
		updated_at = now() WHERE status = 'pending' AND created_at < $1`, olderThan)
	return tag.RowsAffected(), err
}

// String helps logs; it never contains secrets.
func (m Merchant) String() string { return fmt.Sprintf("merchant(%s)", m.ID) }
