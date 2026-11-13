package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// doAs sends a request with another merchant's API key.
func (h *harness) doAs(key, method, path, idemKey, body string) resp {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
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

// TestTenantIsolation: merchant B must not see, change or replay anything
// of merchant A. Every payment route answers 404, as if the id did not
// exist, so B cannot even learn that it exists.
func TestTenantIsolation(t *testing.T) {
	h := newHarness(t, 1000, 1000)
	other, err := h.svc.CreateMerchant(context.Background(), "Other Store", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b := other.APIKey

	tok := h.expect(h.post("/v1/card-tokens", `{"number":"4111 1111 1111 1111","cvv":"123","exp_month":12,"exp_year":2099}`), 201)
	createBody := fmt.Sprintf(`{"amount":1000,"currency":"BRL","method":"card","card_token":%q}`, tok.body["id"])
	sharedKey := newKey()
	a := h.expect(h.do("POST", "/v1/payments", sharedKey, createBody), 201)
	pid := a.body["id"].(string)
	h.expect(h.post("/v1/payments/"+pid+"/authorize", ""), 200)

	// B cannot use A's card token.
	r := h.doAs(b, "POST", "/v1/payments", newKey(), createBody)
	if r.code != 422 || r.body["field"] != "card_token" {
		t.Fatalf("B used A's card token: %d %s", r.code, r.raw)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/payments/" + pid},
		{"GET", "/v1/payments/" + pid + "/events"},
		{"GET", "/v1/payments/" + pid + "/ledger"},
		{"POST", "/v1/payments/" + pid + "/authorize"},
		{"POST", "/v1/payments/" + pid + "/capture"},
		{"POST", "/v1/payments/" + pid + "/void"},
		{"POST", "/v1/payments/" + pid + "/refunds"},
		{"POST", "/v1/simulator/payments/" + pid + "/pix-paid"},
	} {
		key := ""
		if c.method == "POST" {
			key = newKey()
		}
		if r := h.doAs(b, c.method, c.path, key, ""); r.code != 404 {
			t.Errorf("B %s %s: %d %s", c.method, c.path, r.code, r.raw)
		}
	}
	if r := h.doAs(b, "GET", "/v1/payments", "", ""); strings.Contains(r.raw, pid) {
		t.Fatalf("B's list shows A's payment: %s", r.raw)
	}
	// The same Idempotency-Key in another merchant is a different key: B
	// gets its own new resource, never A's stored response.
	r = h.doAs(b, "POST", "/v1/payments", sharedKey, `{"amount":1000,"currency":"BRL","method":"pix"}`)
	if r.code != 201 || r.body["id"] == pid || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("B replayed A's idempotent response: %d %s", r.code, r.raw)
	}
	// A's payment did not change.
	if got := h.expect(h.do("GET", "/v1/payments/"+pid, "", ""), 200); got.body["status"] != "authorized" {
		t.Fatalf("A's payment changed: %s", got.raw)
	}
}
