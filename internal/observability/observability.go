// Package observability sets up structured logs and Prometheus metrics.
//
// This is the only package allowed to use float64: the Prometheus client
// API takes seconds as float64. Money never passes through here.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// sensitiveKeys are log attribute names that are always redacted, as a
// second line of defense. The first line is that handlers never log
// request bodies or card fields at all.
var sensitiveKeys = map[string]bool{
	"number": true, "pan": true, "card_number": true, "cvv": true, "cvc": true,
	"authorization": true, "api_key": true, "secret": true, "webhook_secret": true,
}

type ctxKey struct{}

// WithRequestID stores the request id in ctx. Every log record written
// with that ctx gets a request_id attribute.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the request id stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

type requestIDHandler struct{ slog.Handler }

func (h requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h requestIDHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(as)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}

// NewLogger returns a JSON slog logger that adds the request id from the
// context and redacts sensitive attribute names.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(requestIDHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if sensitiveKeys[strings.ToLower(a.Key)] {
				return slog.String(a.Key, "[REDACTED]")
			}
			return a
		},
	})})
}

// ParseLevel maps "debug", "info", "warn", "error" to a level (default info).
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Metrics holds the application metrics.
type Metrics struct {
	Registry          *prometheus.Registry
	HTTPRequests      *prometheus.CounterVec
	HTTPDuration      *prometheus.HistogramVec
	Transitions       *prometheus.CounterVec
	AcquirerCalls     *prometheus.CounterVec
	AcquirerDuration  *prometheus.HistogramVec
	BreakerState      prometheus.Gauge
	WebhookDeliveries *prometheus.CounterVec
}

// NewMetrics registers all metrics on a fresh registry (no globals).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_http_requests_total", Help: "HTTP requests by route and status.",
		}, []string{"method", "route", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_http_request_duration_seconds", Help: "HTTP request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		Transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_payment_transitions_total", Help: "Payment state transitions.",
		}, []string{"op", "to"}),
		AcquirerCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_acquirer_calls_total", Help: "Provider call attempts by outcome.",
		}, []string{"op", "outcome"}),
		AcquirerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_acquirer_call_duration_seconds", Help: "Provider call latency per attempt.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		BreakerState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_acquirer_circuit_state", Help: "0 closed, 1 open, 2 half-open.",
		}),
		WebhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_webhook_deliveries_total", Help: "Webhook attempts by outcome.",
		}, []string{"outcome"}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.Transitions, m.AcquirerCalls, m.AcquirerDuration,
		m.BreakerState, m.WebhookDeliveries,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// ObserveHTTP records one request.
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	m.HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.HTTPDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// ObserveAcquirer records one provider attempt.
func (m *Metrics) ObserveAcquirer(op, outcome string, d time.Duration) {
	m.AcquirerCalls.WithLabelValues(op, outcome).Inc()
	if d > 0 {
		m.AcquirerDuration.WithLabelValues(op).Observe(d.Seconds())
	}
}

// SetBreakerState records the circuit breaker state (0, 1, 2).
func (m *Metrics) SetBreakerState(state int) { m.BreakerState.Set(float64(state)) }
