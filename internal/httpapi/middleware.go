package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kmadsdev/simple-payment-gateway/internal/id"
	"github.com/kmadsdev/simple-payment-gateway/internal/observability"
	"github.com/kmadsdev/simple-payment-gateway/internal/service"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

const maxBodyBytes = 64 << 10

type merchantKey struct{}

func merchantFrom(ctx context.Context) store.Merchant {
	m, _ := ctx.Value(merchantKey{}).(store.Merchant)
	return m
}

// accessInfo lets inner middleware report data for the access log line.
type accessInfo struct{ merchantID string }

type accessKey struct{}

// statusRecorder remembers the status code and, when buf is set, a copy
// of the body.
type statusRecorder struct {
	http.ResponseWriter
	status int
	buf    *bytes.Buffer
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.buf != nil {
		s.buf.Write(b)
	}
	return s.ResponseWriter.Write(b)
}

func validRequestID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// observe sets the request id, recovers panics, and writes one access log
// line and the HTTP metrics. It never logs bodies or headers.
func (a *API) observe(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if !validRequestID(rid) {
			rid = id.New("req")
		}
		info := &accessInfo{}
		ctx := context.WithValue(observability.WithRequestID(r.Context(), rid), accessKey{}, info)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", rid)
		rec := &statusRecorder{ResponseWriter: w}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		defer func() {
			if p := recover(); p != nil {
				a.Log.ErrorContext(ctx, "panic", "route", route)
				if rec.status == 0 {
					writeProblem(rec, r, http.StatusInternalServerError, "internal_error", "Internal error", "")
				}
			}
			d := time.Since(start)
			if a.Metrics != nil {
				a.Metrics.ObserveHTTP(r.Method, route, rec.status, d)
			}
			a.Log.InfoContext(ctx, "http request", "method", r.Method, "route", route, "status", rec.status,
				"duration_ms", d.Milliseconds(), "merchant_id", info.merchantID)
		}()
		next(rec, r)
	}
}

// authenticate checks "Authorization: Bearer <api key>" and applies the
// per-merchant rate limit.
func (a *API) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gateway"`)
			writeProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Missing or invalid API key", "")
			return
		}
		m, err := a.Svc.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, service.ErrUnauthenticated) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="gateway"`)
				writeProblem(w, r, http.StatusUnauthorized, "unauthenticated", "Missing or invalid API key", "")
				return
			}
			a.writeError(w, r, err)
			return
		}
		if ok, wait := a.Limiter.Allow(m.ID); !ok {
			secs := int(wait / time.Second)
			if wait%time.Second != 0 {
				secs++
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeProblem(w, r, http.StatusTooManyRequests, "rate_limited", "Too many requests", "")
			return
		}
		if info, ok := r.Context().Value(accessKey{}).(*accessInfo); ok {
			info.merchantID = m.ID
		}
		next(w, r.WithContext(context.WithValue(r.Context(), merchantKey{}, m)))
	}
}

// idempotent implements the Idempotency-Key header for mutating requests.
//
//   - a new key: run the handler and store the response (except 5xx and
//     409, which release the key so the client can retry)
//   - same key, same request, finished: replay the stored response
//   - same key, same request, still running: 409
//   - same key, different request: 422
func (a *API) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 255 {
			writeProblem(w, r, http.StatusBadRequest, "idempotency_key_required",
				"Idempotency-Key header is required", "Send a unique key (1 to 255 characters) per operation.")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		// The fingerprint is an HMAC keyed with the caller's API key, not a
		// plain SHA-256. A tokenization body holds a card number and a CVV,
		// which have little entropy: a plain hash in the database could be
		// brute-forced. The API key is never stored, so a database copy
		// alone cannot do that. A retry with the same key still matches.
		apiKey, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		h := hmac.New(sha256.New, []byte(apiKey))
		h.Write([]byte(r.Method + " " + r.URL.Path + "\n"))
		h.Write(body)
		hash := h.Sum(nil)

		m := merchantFrom(r.Context())
		rec, created, err := a.Store.IdempotencyBegin(r.Context(), m.ID, key, hash)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if !created {
			switch {
			case !bytes.Equal(rec.RequestHash, hash):
				writeProblem(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused",
					"Idempotency-Key was used with a different request", "Use a new key for a new operation.")
			case rec.Status != "completed":
				writeProblem(w, r, http.StatusConflict, "idempotency_request_in_progress",
					"A request with this Idempotency-Key is still running", "Retry in a moment.")
			default:
				ct := "application/json"
				if rec.ResponseCode >= 400 {
					ct = "application/problem+json"
				}
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(rec.ResponseCode)
				_, _ = w.Write(rec.ResponseBody)
			}
			return
		}

		sr := &statusRecorder{ResponseWriter: w, buf: &bytes.Buffer{}}
		next(sr, r)

		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		if sr.status >= 500 || sr.status == http.StatusConflict {
			err = a.Store.IdempotencyRelease(ctx, m.ID, key)
		} else {
			err = a.Store.IdempotencyComplete(ctx, m.ID, key, sr.status, sr.buf.Bytes())
		}
		if err != nil {
			a.Log.ErrorContext(ctx, "save idempotency result", "error", err.Error())
		}
	}
}
