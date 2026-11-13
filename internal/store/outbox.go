package store

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutboxMessage is one webhook to deliver, joined with the merchant's
// endpoint and signing secret.
type OutboxMessage struct {
	ID            string
	MerchantID    string
	PaymentID     string
	EventType     string
	Payload       []byte
	Attempt       int // 1-based number of the attempt this claim is for
	WebhookURL    string
	WebhookSecret string
	CreatedAt     time.Time
}

// ClaimOutbox picks due messages and pushes their next_attempt_at forward
// by lease, so other workers skip them while this one delivers. FOR UPDATE
// SKIP LOCKED lets several workers run side by side without double
// delivery inside one lease. No lock is held during the HTTP call.
func (s *Store) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]OutboxMessage, error) {
	rows, err := s.Pool.Query(ctx, `WITH due AS (
			SELECT id FROM outbox WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		UPDATE outbox o SET next_attempt_at = now() + ($2 * interval '1 millisecond'), attempts = o.attempts + 1
		FROM due, merchants m WHERE o.id = due.id AND m.id = o.merchant_id
		RETURNING o.id, o.merchant_id, o.payment_id, o.event_type, o.payload::text, o.attempts,
			coalesce(m.webhook_url, ''), m.webhook_secret, o.created_at`, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	msgs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutboxMessage, error) {
		var m OutboxMessage
		var payload string
		err := r.Scan(&m.ID, &m.MerchantID, &m.PaymentID, &m.EventType, &payload, &m.Attempt, &m.WebhookURL,
			&m.WebhookSecret, &m.CreatedAt)
		m.Payload = []byte(payload)
		return m, err
	})
	// UPDATE ... RETURNING has no defined order. Deliver oldest first.
	// Order is still best effort across retries; receivers must not rely on it.
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt.Before(msgs[j].CreatedAt) })
	return msgs, err
}

// DeliveryResult says what to do with a message after an attempt.
type DeliveryResult struct {
	StatusCode int    // 0 when no HTTP response
	Error      string // empty on success
	Duration   time.Duration
	// Outcome is delivered, retry, failed or skipped.
	Outcome   string
	NextDelay time.Duration // for retry
}

// RecordDelivery logs the attempt and updates the message, in one transaction.
func (s *Store) RecordDelivery(ctx context.Context, m OutboxMessage, r DeliveryResult) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if r.Outcome != "skipped" {
			if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (outbox_id, attempt, status_code, error, duration_ms)
				VALUES ($1, $2, NULLIF($3, 0), NULLIF($4, ''), $5)`,
				m.ID, m.Attempt, r.StatusCode, r.Error, r.Duration.Milliseconds()); err != nil {
				return err
			}
		}
		var err error
		switch r.Outcome {
		case "delivered":
			_, err = tx.Exec(ctx, `UPDATE outbox SET status = 'delivered', delivered_at = now(), last_error = NULL WHERE id = $1`, m.ID)
		case "retry":
			_, err = tx.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() + ($2 * interval '1 millisecond'), last_error = $3
				WHERE id = $1`, m.ID, r.NextDelay.Milliseconds(), r.Error)
		case "failed":
			_, err = tx.Exec(ctx, `UPDATE outbox SET status = 'failed', last_error = $2 WHERE id = $1`, m.ID, r.Error)
		case "skipped":
			_, err = tx.Exec(ctx, `UPDATE outbox SET status = 'skipped' WHERE id = $1`, m.ID)
		}
		return err
	})
}

// OutboxPending counts messages waiting for delivery (for the metrics).
func (s *Store) OutboxPending(ctx context.Context) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE status = 'pending'`).Scan(&n)
	return n, err
}
