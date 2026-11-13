// Package webhook signs and delivers events to merchants.
//
// Signature header format (one header, comma separated):
//
//	Webhook-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>
//
// The receiver recomputes the HMAC over the raw body, compares it in
// constant time, and rejects timestamps outside a tolerance window to
// block replays. examples/webhook-receiver shows the receiver side.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header names.
const (
	HeaderSignature = "Webhook-Signature"
	HeaderID        = "Webhook-Id"
)

// Verification errors.
var (
	ErrBadHeader    = errors.New("malformed signature header")
	ErrBadSignature = errors.New("signature mismatch")
	ErrTooOld       = errors.New("timestamp outside tolerance")
)

func mac(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign returns the header value for body at time ts.
func Sign(secret string, ts time.Time, body []byte) string {
	t := ts.Unix()
	return "t=" + strconv.FormatInt(t, 10) + ",v1=" + mac(secret, t, body)
}

// Verify checks a header value against body. tolerance bounds the age
// (and the clock skew) of the timestamp.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrBadHeader
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return ErrBadHeader
			}
			ts = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return ErrBadHeader
	}
	age := now.Sub(time.Unix(ts, 0))
	if age > tolerance || age < -tolerance {
		return ErrTooOld
	}
	want := []byte(mac(secret, ts, body))
	for _, s := range sigs { // several v1 values allow secret rotation
		if hmac.Equal([]byte(s), want) {
			return nil
		}
	}
	return ErrBadSignature
}
