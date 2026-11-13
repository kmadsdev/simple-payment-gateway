// Command webhook-receiver is a tiny merchant-side endpoint that verifies
// the gateway's webhook signature. It uses only the standard library so a
// merchant can copy the verify function as is.
//
//	WEBHOOK_SECRET=whsec_... ADDR=:8081 go run ./examples/webhook-receiver
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tolerance = 5 * time.Minute

// verify checks "Webhook-Signature: t=<unix>,v1=<hex>" against the raw body.
func verify(secret, header string, body []byte, now time.Time) error {
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return errors.New("malformed signature header")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside tolerance (possible replay)")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

// receiver de-duplicates by event id: delivery is at least once.
type receiver struct {
	secret string
	log    *slog.Logger
	mu     sync.Mutex
	seen   map[string]bool
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := verify(rc.secret, r.Header.Get("Webhook-Signature"), body, time.Now()); err != nil {
		rc.log.Warn("rejected webhook", "reason", err.Error())
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var evt struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rc.mu.Lock()
	dup := rc.seen[evt.ID]
	rc.seen[evt.ID] = true
	rc.mu.Unlock()
	if dup {
		rc.log.Info("duplicate webhook ignored", "event_id", evt.ID)
	} else {
		rc.log.Info("webhook received", "event_id", evt.ID, "type", evt.Type, "payment_id", evt.Data.ID, "status", evt.Data.Status)
	}
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		log.Error("WEBHOOK_SECRET is not set")
		os.Exit(1)
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8081"
	}
	srv := &http.Server{Addr: addr, Handler: &receiver{secret: secret, log: log, seen: map[string]bool{}},
		ReadHeaderTimeout: 5 * time.Second}
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server", "error", err.Error())
		os.Exit(1)
	}
}
