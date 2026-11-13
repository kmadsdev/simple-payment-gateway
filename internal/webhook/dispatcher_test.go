package webhook

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// fakeOutbox hands out msgs once and keeps the recorded results. Like the
// real store, it fails to record when ctx is done.
type fakeOutbox struct {
	mu       sync.Mutex
	msgs     []store.OutboxMessage
	recorded map[string]store.DeliveryResult
}

func (f *fakeOutbox) ClaimOutbox(ctx context.Context, _ int, _ time.Duration) ([]store.OutboxMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs
	f.msgs = nil
	return m, nil
}

func (f *fakeOutbox) RecordDelivery(ctx context.Context, m store.OutboxMessage, r store.DeliveryResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded[m.ID] = r
	return nil
}

func newDispatcher(out Outbox) *Dispatcher {
	return &Dispatcher{Outbox: out, Client: NewClient(5 * time.Second), MaxAttempts: 3, Batch: 10, Lease: time.Minute,
		Backoff: resilience.Backoff{Base: time.Second, Max: time.Second},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func msg(id, url string) store.OutboxMessage {
	return store.OutboxMessage{ID: id, Payload: []byte(`{}`), Attempt: 1, WebhookURL: url, WebhookSecret: "whsec_x"}
}

// TestDispatcherDoesNotFollowRedirects: a merchant endpoint that answers
// 3xx must not bounce the gateway (and the signed payload) to another host,
// for example an internal address (SSRF). The 3xx is a failed attempt.
func TestDispatcherDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer inner.Close()
	bouncer := httptest.NewServer(http.RedirectHandler(inner.URL, http.StatusTemporaryRedirect))
	defer bouncer.Close()

	out := &fakeOutbox{msgs: []store.OutboxMessage{msg("evt_1", bouncer.URL)}, recorded: map[string]store.DeliveryResult{}}
	if _, err := newDispatcher(out).ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
	if r := out.recorded["evt_1"]; r.Outcome != "retry" || r.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("result = %+v, want retry with status 307", r)
	}
}

// TestDispatcherDrainsOnShutdown: when the worker context ends during a
// delivery, that delivery finishes and its result is recorded, and no new
// delivery starts. Before, the in-flight request was cut, the attempt was
// burned and the result could not be written.
func TestDispatcherDrainsOnShutdown(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out := &fakeOutbox{msgs: []store.OutboxMessage{msg("evt_1", srv.URL), msg("evt_2", srv.URL)},
		recorded: map[string]store.DeliveryResult{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = newDispatcher(out).ProcessOnce(ctx)
	}()
	<-entered
	cancel() // shutdown starts while evt_1 is in flight
	close(release)
	<-done

	if r, ok := out.recorded["evt_1"]; !ok || r.Outcome != "delivered" {
		t.Fatalf("in-flight delivery: recorded=%v result=%+v, want delivered", ok, r)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d deliveries started, want 1 (no new work after shutdown)", hits.Load())
	}
	if _, ok := out.recorded["evt_2"]; ok {
		t.Fatal("evt_2 must stay claimed and be retried after the lease")
	}
}
