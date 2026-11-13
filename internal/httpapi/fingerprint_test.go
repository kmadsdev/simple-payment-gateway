package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
)

// TestIdempotencyFingerprintIsKeyed: the stored request fingerprint of a
// card tokenization covers the card number and the CVV. A plain SHA-256
// of the body could be brute-forced from a database copy (a card number
// has little entropy once the BIN and the Luhn digit are known), so the
// stored value must not be the plain hash.
func TestIdempotencyFingerprintIsKeyed(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	key := newKey()
	body := `{"number":"` + testPAN + `","cvv":"123","exp_month":12,"exp_year":2099}`
	h.expect(h.do("POST", "/v1/card-tokens", key, body), 201)
	var stored []byte
	if err := h.svc.Store.Pool.QueryRow(context.Background(),
		`SELECT request_hash FROM idempotency_keys WHERE key = $1`, key).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256([]byte("POST /v1/card-tokens\n" + body))
	if bytes.Equal(stored, plain[:]) {
		t.Fatal("idempotency_keys.request_hash is an unkeyed SHA-256 of a body with the card number and CVV")
	}
	// A retry still matches, and a changed body still does not.
	r := h.expect(h.do("POST", "/v1/card-tokens", key, body), 201)
	if r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("retry was not replayed")
	}
	h.expect(h.do("POST", "/v1/card-tokens", key, strings.Replace(body, "123", "124", 1)), 422)
}
