package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/acquirer"
	"github.com/kmadsdev/simple-payment-gateway/internal/httpapi"
	"github.com/kmadsdev/simple-payment-gateway/internal/observability"
	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
	"github.com/kmadsdev/simple-payment-gateway/internal/service"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
	"github.com/kmadsdev/simple-payment-gateway/internal/testdb"
	"github.com/kmadsdev/simple-payment-gateway/internal/webhook"
)

const testPAN = "4111111111111111"

// lockedBuffer is a log sink safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	svc    *service.Service
	key    string
	secret string
	logs   *lockedBuffer
	hooks  *hookReceiver
	disp   *webhook.Dispatcher
}

type hookReceiver struct {
	mu       sync.Mutex
	secret   string
	events   []string
	failNext atomic.Int32
	srv      *httptest.Server
}

func newHookReceiver(t *testing.T) *hookReceiver {
	h := &hookReceiver{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.failNext.Load() > 0 {
			h.failNext.Add(-1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := webhook.Verify(h.secret, r.Header.Get(webhook.HeaderSignature), body, time.Now(), 5*time.Minute); err != nil {
			t.Errorf("webhook signature: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var evt struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal(body, &evt)
		if evt.ID != r.Header.Get(webhook.HeaderID) {
			t.Errorf("Webhook-Id %q != body id %q", r.Header.Get(webhook.HeaderID), evt.ID)
		}
		h.mu.Lock()
		h.events = append(h.events, evt.Type)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookReceiver) received() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func newHarness(t *testing.T, rate, burst int) *harness {
	t.Helper()
	pool := testdb.New(t)
	logs := &lockedBuffer{}
	log := observability.NewLogger(logs, slog.LevelDebug)
	sim := acquirer.NewSimulator(time.Second)
	svc := &service.Service{
		Store: store.New(pool),
		Acq: &acquirer.Resilient{Inner: sim, AttemptTimeout: 50 * time.Millisecond, TotalBudget: time.Second, MaxAttempts: 3,
			Backoff: resilience.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
			Breaker: &resilience.Breaker{Threshold: 100, Cooldown: time.Second}},
		Sim: sim,
		Cfg: service.Config{AuthorizationTTL: time.Hour, PendingTTL: time.Hour, PixMerchantName: "Test Store", PixCity: "Sao Paulo"},
		Log: log,
	}
	hooks := newHookReceiver(t)
	nm, err := svc.CreateMerchant(context.Background(), "Test Store", hooks.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	hooks.secret = nm.WebhookSecret
	var draining atomic.Bool
	api := &httpapi.API{Svc: svc, Store: svc.Store, Limiter: httpapi.NewRateLimiter(rate, burst, nil),
		Metrics: observability.NewMetrics(), Log: log, Simulator: true, Draining: &draining}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	disp := &webhook.Dispatcher{Outbox: svc.Store, Client: srv.Client(), MaxAttempts: 3, Batch: 50, Lease: time.Minute,
		Backoff: resilience.Backoff{Base: time.Second, Max: time.Second}, Log: log}
	return &harness{t: t, srv: srv, svc: svc, key: nm.APIKey, secret: nm.WebhookSecret, logs: logs, hooks: hooks, disp: disp}
}

type resp struct {
	code   int
	header http.Header
	body   map[string]any
	raw    string
}

func (h *harness) do(method, path, idemKey, body string) resp {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.key)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{code: res.StatusCode, header: res.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

var keyN atomic.Int64

func newKey() string { return fmt.Sprintf("key-%d-%d", time.Now().UnixNano(), keyN.Add(1)) }

func (h *harness) post(path, body string) resp { return h.do("POST", path, newKey(), body) }

func (h *harness) expect(r resp, code int) resp {
	h.t.Helper()
	if r.code != code {
		h.t.Fatalf("status %d, want %d: %s", r.code, code, r.raw)
	}
	return r
}

func (h *harness) cardPayment(amount int) string {
	tok := h.expect(h.post("/v1/card-tokens", `{"number":"4111 1111 1111 1111","cvv":"123","exp_month":12,"exp_year":2099}`), 201)
	p := h.expect(h.post("/v1/payments", fmt.Sprintf(`{"amount":%d,"currency":"BRL","method":"card","card_token":%q}`,
		amount, tok.body["id"])), 201)
	return p.body["id"].(string)
}

func TestCardCheckoutFlowWithWebhooks(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	pid := h.cardPayment(15990)

	r := h.expect(h.post("/v1/payments/"+pid+"/authorize", ""), 200)
	if r.body["status"] != "authorized" || r.body["authorized_amount"].(float64) != 15990 {
		t.Fatalf("authorize: %s", r.raw)
	}
	r = h.expect(h.post("/v1/payments/"+pid+"/capture", `{"amount":15000}`), 200)
	if r.body["status"] != "captured" || r.body["captured_amount"].(float64) != 15000 {
		t.Fatalf("capture: %s", r.raw)
	}
	r = h.expect(h.post("/v1/payments/"+pid+"/refunds", `{"amount":5000}`), 200)
	if r.body["status"] != "partially_refunded" {
		t.Fatalf("refund: %s", r.raw)
	}
	r = h.expect(h.post("/v1/payments/"+pid+"/refunds", `{"amount":10001}`), 422)
	if r.body["code"] != "amount_too_high" {
		t.Fatalf("over-refund: %s", r.raw)
	}
	r = h.expect(h.post("/v1/payments/"+pid+"/refunds", ""), 200) // the rest
	if r.body["status"] != "refunded" || r.body["refunded_amount"].(float64) != 15000 {
		t.Fatalf("refund rest: %s", r.raw)
	}

	ev := h.expect(h.do("GET", "/v1/payments/"+pid+"/events", "", ""), 200)
	if n := len(ev.body["data"].([]any)); n != 5 { // create, authorize, capture, refund, refund
		t.Fatalf("events = %d: %s", n, ev.raw)
	}
	led := h.expect(h.do("GET", "/v1/payments/"+pid+"/ledger", "", ""), 200)
	var sum float64
	for _, e := range led.body["data"].([]any) {
		sum += e.(map[string]any)["amount"].(float64)
	}
	if sum != 0 {
		t.Fatalf("ledger sum = %v", sum)
	}

	if _, err := h.disp.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"payment.authorized", "payment.captured", "payment.partially_refunded", "payment.refunded"}
	if got := h.hooks.received(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("webhooks = %v, want %v", got, want)
	}
}

func TestDeclineAndIllegalTransition(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	pid := h.cardPayment(1051)
	r := h.expect(h.post("/v1/payments/"+pid+"/capture", ""), 409)
	if r.body["code"] != "invalid_state_transition" || r.header.Get("Content-Type") != "application/problem+json" ||
		!strings.HasSuffix(r.body["type"].(string), "#invalid_state_transition") || r.body["request_id"] == "" {
		t.Fatalf("problem: %s", r.raw)
	}
	r = h.expect(h.post("/v1/payments/"+pid+"/authorize", ""), 200)
	if r.body["status"] != "declined" || r.body["decline_code"] != "insufficient_funds" {
		t.Fatalf("decline: %s", r.raw)
	}
	h.expect(h.post("/v1/payments/"+pid+"/void", ""), 409)
	h.expect(h.do("GET", "/v1/payments/pay_doesnotexist", "", ""), 404)
}

func TestProviderUnavailableReturns503(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	pid := h.cardPayment(1091)
	key := newKey()
	r := h.expect(h.do("POST", "/v1/payments/"+pid+"/authorize", key, ""), 503)
	if r.header.Get("Retry-After") == "" || r.body["code"] != "provider_unavailable" {
		t.Fatalf("503: %s", r.raw)
	}
	// 5xx releases the idempotency key: the same key may be retried.
	h.expect(h.do("POST", "/v1/payments/"+pid+"/authorize", key, ""), 503)
}

func TestIdempotencyReplayAndMismatch(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	key := newKey()
	body := `{"amount":1000,"currency":"BRL","method":"pix"}`
	a := h.expect(h.do("POST", "/v1/payments", key, body), 201)
	b := h.expect(h.do("POST", "/v1/payments", key, body), 201)
	if a.body["id"] != b.body["id"] || b.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs: %s vs %s", a.raw, b.raw)
	}
	c := h.expect(h.do("POST", "/v1/payments", key, `{"amount":2000,"currency":"BRL","method":"pix"}`), 422)
	if c.body["code"] != "idempotency_key_reused" {
		t.Fatalf("mismatch: %s", c.raw)
	}
	d := h.expect(h.do("POST", "/v1/payments", "", body), 400)
	if d.body["code"] != "idempotency_key_required" {
		t.Fatalf("missing key: %s", d.raw)
	}
	// Validation errors are stored and replayed too.
	k2 := newKey()
	h.expect(h.do("POST", "/v1/payments", k2, `{"amount":0,"currency":"BRL","method":"pix"}`), 422)
	e := h.expect(h.do("POST", "/v1/payments", k2, `{"amount":0,"currency":"BRL","method":"pix"}`), 422)
	if e.header.Get("Idempotent-Replayed") != "true" || e.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("replayed problem: %v", e.header)
	}
}

// TestConcurrentDuplicateRequests sends the same request with the same key
// from two goroutines at once (many rounds). Exactly one payment must exist
// per key, and both answers are either the created payment or a 409.
func TestConcurrentDuplicateRequests(t *testing.T) {
	h := newHarness(t, 10000, 10000)
	for round := 0; round < 20; round++ {
		key := newKey()
		body := fmt.Sprintf(`{"amount":%d,"currency":"BRL","method":"pix","reference":%q}`, 1000+round, key)
		var wg sync.WaitGroup
		results := make([]resp, 2)
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = h.do("POST", "/v1/payments", key, body)
			}()
		}
		close(start)
		wg.Wait()
		created := 0
		for _, r := range results {
			switch r.code {
			case 201:
				created++
			case 409:
				if r.body["code"] != "idempotency_request_in_progress" {
					t.Fatalf("409 code: %s", r.raw)
				}
			default:
				t.Fatalf("unexpected status %d: %s", r.code, r.raw)
			}
		}
		if created == 0 {
			t.Fatal("no request created the payment")
		}
		if created == 2 && results[0].body["id"] != results[1].body["id"] {
			t.Fatal("two different payments for one key")
		}
		var n int
		_ = h.svc.Store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE reference = $1`, key).Scan(&n)
		if n != 1 {
			t.Fatalf("round %d: %d payments for one key", round, n)
		}
	}
}

// TestLogsAndStorageNeverContainCardData proves that the full card number
// and the CVV never reach the logs, the error responses or any table.
func TestLogsAndStorageNeverContainCardData(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	const cvv = "987"
	bodies := []struct {
		body string
		code int
	}{
		{`{"number":"` + testPAN + `","cvv":"` + cvv + `","exp_month":12,"exp_year":2099}`, 201},
		{`{"number":"4111111111111112","cvv":"` + cvv + `","exp_month":12,"exp_year":2099}`, 422}, // bad Luhn
		{`{"number":"` + testPAN + `","cvv":"98","exp_month":12,"exp_year":2099}`, 422},           // bad CVV
		{`{"number":"` + testPAN + `","cvv":"` + cvv + `","exp_month":1,"exp_year":2001}`, 422},   // expired
		{`{"number":"` + testPAN + `","cvv":"` + cvv + `","unknown":1}`, 400},                     // unknown field
		{`{"number":` + testPAN + `,"cvv":"` + cvv + `"}`, 400},                                   // wrong type
	}
	var responses strings.Builder
	for _, b := range bodies {
		r := h.expect(h.post("/v1/card-tokens", b.body), b.code)
		responses.WriteString(r.raw)
	}
	pid := h.cardPayment(1000)
	h.expect(h.post("/v1/payments/"+pid+"/authorize", ""), 200)
	_, _ = h.disp.ProcessOnce(context.Background())

	logs := h.logs.String()
	if !strings.Contains(logs, `"route":"POST /v1/card-tokens"`) {
		t.Fatalf("expected access logs, got: %s", logs)
	}
	for _, secret := range []string{testPAN, "4111111111111112", `"` + cvv + `"`, "1111 1111"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log contains %q", secret)
		}
		if strings.Contains(responses.String(), secret) {
			t.Fatalf("response contains %q", secret)
		}
	}
	// Scan every row of every table as text.
	rows, err := h.svc.Store.Pool.Query(context.Background(),
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 8 {
		t.Fatalf("tables = %v", tables)
	}
	for _, tbl := range tables {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %q t WHERE t::text LIKE '%%' || $1 || '%%'`, tbl)
		if err := h.svc.Store.Pool.QueryRow(context.Background(), q, testPAN).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("table %s contains the card number", tbl)
		}
	}
}

func TestPixFlow(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	p := h.expect(h.post("/v1/payments", `{"amount":4990,"currency":"BRL","method":"pix","description":"Order 42"}`), 201)
	pid := p.body["id"].(string)
	r := h.expect(h.post("/v1/payments/"+pid+"/authorize", ""), 200)
	pixInfo := r.body["pix"].(map[string]any)
	if r.body["status"] != "pending" || !strings.Contains(pixInfo["copy_paste"].(string), "example.simulated.pix") {
		t.Fatalf("pix pending: %s", r.raw)
	}
	h.expect(h.post("/v1/payments/"+pid+"/capture", ""), 409) // Pix has no capture step
	r = h.expect(h.post("/v1/simulator/payments/"+pid+"/pix-paid", ""), 200)
	if r.body["status"] != "captured" || r.body["captured_amount"].(float64) != 4990 {
		t.Fatalf("pix paid: %s", r.raw)
	}
	h.expect(h.post("/v1/payments/"+pid+"/refunds", `{"amount":990}`), 200)
}

func TestAuthAndRateLimit(t *testing.T) {
	h := newHarness(t, 1, 2)
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/payments", nil)
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no key: %d", res.StatusCode)
	}
	h.expect(h.do("GET", "/v1/payments", "", ""), 200)
	h.expect(h.do("GET", "/v1/payments", "", ""), 200)
	r := h.expect(h.do("GET", "/v1/payments", "", ""), 429)
	if r.header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", r.header.Get("Retry-After"))
	}
}

func TestPagination(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	for i := 0; i < 5; i++ {
		h.expect(h.post("/v1/payments", fmt.Sprintf(`{"amount":%d,"currency":"BRL","method":"pix"}`, 100+i)), 201)
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/v1/payments?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := h.expect(h.do("GET", path, "", ""), 200)
		pages++
		for _, d := range r.body["data"].([]any) {
			seen[d.(map[string]any)["id"].(string)] = true
		}
		if r.body["has_more"] != true {
			break
		}
		cursor = r.body["next_cursor"].(string)
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("seen %d in %d pages", len(seen), pages)
	}
	h.expect(h.do("GET", "/v1/payments?limit=0", "", ""), 422)
	h.expect(h.do("GET", "/v1/payments?cursor=@@", "", ""), 422)
}

func TestWebhookRetryThenDelivered(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	pid := h.cardPayment(1000)
	h.expect(h.post("/v1/payments/"+pid+"/authorize", ""), 200)
	h.hooks.failNext.Store(1)
	ctx := context.Background()
	if _, err := h.disp.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pool := h.svc.Store.Pool
	var status string
	var attempts int
	_ = pool.QueryRow(ctx, `SELECT status, attempts FROM outbox WHERE payment_id = $1`, pid).Scan(&status, &attempts)
	if status != "pending" || attempts != 1 {
		t.Fatalf("after failure: %s %d", status, attempts)
	}
	if n, _ := h.disp.ProcessOnce(ctx); n != 0 {
		t.Fatal("message must wait for its backoff")
	}
	_, _ = pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE payment_id = $1`, pid)
	if _, err := h.disp.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	_ = pool.QueryRow(ctx, `SELECT status FROM outbox WHERE payment_id = $1`, pid).Scan(&status)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries d JOIN outbox o ON o.id = d.outbox_id
		WHERE o.payment_id = $1`, pid).Scan(&deliveries)
	if status != "delivered" || deliveries != 2 {
		t.Fatalf("status %s deliveries %d", status, deliveries)
	}
}

func TestProbesAndMetrics(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		res, err := h.srv.Client().Get(h.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
		if p == "/metrics" && !strings.Contains(string(body), "gateway_http_requests_total") {
			t.Fatal("metrics missing")
		}
	}
	h.expect(h.do("GET", "/nope", "", ""), 404)
}
