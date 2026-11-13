package webhook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// Outbox is the part of the store the dispatcher needs.
type Outbox interface {
	ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]store.OutboxMessage, error)
	RecordDelivery(ctx context.Context, m store.OutboxMessage, r store.DeliveryResult) error
}

// Dispatcher delivers outbox messages to merchant endpoints.
//
// Delivery is at least once: a merchant can get the same event twice (for
// example when the response is lost), so receivers de-duplicate by the
// Webhook-Id header.
type Dispatcher struct {
	Outbox      Outbox
	Client      *http.Client
	Backoff     resilience.Backoff
	MaxAttempts int
	Batch       int
	Lease       time.Duration
	Now         func() time.Time
	Log         *slog.Logger
	OnResult    func(outcome string) // metrics hook, optional
}

// ProcessOnce claims one batch and tries each message once. It returns
// how many messages it handled.
func (d *Dispatcher) ProcessOnce(ctx context.Context) (int, error) {
	msgs, err := d.Outbox.ClaimOutbox(ctx, d.Batch, d.Lease)
	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		r := d.deliver(ctx, m)
		if d.OnResult != nil {
			d.OnResult(r.Outcome)
		}
		if err := d.Outbox.RecordDelivery(ctx, m, r); err != nil {
			d.Log.ErrorContext(ctx, "record webhook delivery", "event_id", m.ID, "error", err)
		}
	}
	return len(msgs), nil
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) deliver(ctx context.Context, m store.OutboxMessage) store.DeliveryResult {
	if m.WebhookURL == "" {
		return store.DeliveryResult{Outcome: "skipped"}
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.WebhookURL, bytes.NewReader(m.Payload))
	if err != nil {
		return store.DeliveryResult{Outcome: "failed", Error: "invalid webhook url"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "simple-payment-gateway-webhooks/1")
	req.Header.Set(HeaderID, m.ID)
	req.Header.Set(HeaderSignature, Sign(m.WebhookSecret, d.now(), m.Payload))

	r := store.DeliveryResult{}
	resp, err := d.Client.Do(req)
	r.Duration = time.Since(start)
	if err != nil {
		r.Error = "request failed: " + err.Error()
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		r.StatusCode = resp.StatusCode
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			r.Outcome = "delivered"
			return r
		}
		r.Error = "unexpected status " + strconv.Itoa(resp.StatusCode)
	}
	if m.Attempt >= d.MaxAttempts {
		r.Outcome = "failed"
		d.Log.WarnContext(ctx, "webhook gave up", "event_id", m.ID, "attempts", m.Attempt)
		return r
	}
	r.Outcome = "retry"
	r.NextDelay = d.Backoff.Delay(m.Attempt - 1)
	return r
}

// Run processes batches until ctx ends. When a batch is full it continues
// at once; otherwise it waits for interval.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	for {
		n, err := d.ProcessOnce(ctx)
		if err != nil && ctx.Err() == nil {
			d.Log.ErrorContext(ctx, "webhook batch", "error", err)
		}
		if n == d.Batch && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
