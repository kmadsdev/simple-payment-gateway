package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/webhook"
)

// TestVerifyMatchesGatewaySigner cross-checks the standalone verify
// function against the signer the gateway uses.
func TestVerifyMatchesGatewaySigner(t *testing.T) {
	now := time.Now()
	body := []byte(`{"id":"evt_1","type":"payment.captured"}`)
	h := webhook.Sign("whsec_x", now, body)
	if err := verify("whsec_x", h, body, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := verify("whsec_x", h, []byte(`{"id":"evt_1","type":"payment.refunded"}`), now); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := verify("whsec_x", h, body, now.Add(10*time.Minute)); err == nil {
		t.Fatal("replay outside the window accepted")
	}
}

func TestReceiverHandler(t *testing.T) {
	rc := &receiver{secret: "whsec_x", log: slog.New(slog.NewTextHandler(io.Discard, nil)), seen: map[string]bool{}}
	body := `{"id":"evt_1","type":"payment.captured","data":{"id":"pay_1","status":"captured"}}`
	send := func(sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks", strings.NewReader(body))
		req.Header.Set("Webhook-Signature", sig)
		rec := httptest.NewRecorder()
		rc.ServeHTTP(rec, req)
		return rec.Code
	}
	good := webhook.Sign("whsec_x", time.Now(), []byte(body))
	if code := send(good); code != http.StatusNoContent {
		t.Fatalf("good: %d", code)
	}
	if code := send(good); code != http.StatusNoContent || !rc.seen["evt_1"] {
		t.Fatalf("duplicate: %d", code)
	}
	if code := send("t=1,v1=00"); code != http.StatusUnauthorized {
		t.Fatalf("bad: %d", code)
	}
}
