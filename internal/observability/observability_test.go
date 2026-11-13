package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoggerAddsRequestIDAndRedacts(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug)
	ctx := WithRequestID(context.Background(), "req_123")
	log.With("component", "test").InfoContext(ctx, "hello", "cvv", "123", "number", "4111111111111111", "api_key", "sk_x")
	out := buf.String()
	for _, leaked := range []string{"4111111111111111", `"123"`, "sk_x"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("log leaked %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, `"request_id":"req_123"`) || !strings.Contains(out, `"component":"test"`) {
		t.Fatalf("missing attrs: %s", out)
	}
}

func TestParseLevel(t *testing.T) {
	if ParseLevel("debug") != slog.LevelDebug || ParseLevel("nonsense") != slog.LevelInfo {
		t.Fatal("ParseLevel")
	}
}

func TestMetricsRegister(t *testing.T) {
	m := NewMetrics()
	m.ObserveHTTP("GET", "/x", 200, time.Millisecond)
	m.ObserveAcquirer("authorize", "ok", time.Millisecond)
	m.SetBreakerState(1)
	if _, err := m.Registry.Gather(); err != nil {
		t.Fatal(err)
	}
}
