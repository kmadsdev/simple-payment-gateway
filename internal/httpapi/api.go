// Package httpapi is the REST adapter: routes, middleware, handlers and
// RFC 7807 errors.
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
	"github.com/kmadsdev/simple-payment-gateway/internal/observability"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/service"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// API holds the dependencies of the handlers.
type API struct {
	Svc       *service.Service
	Store     *store.Store
	Limiter   *RateLimiter
	Metrics   *observability.Metrics // optional
	Log       *slog.Logger
	Simulator bool         // exposes /v1/simulator routes
	Draining  *atomic.Bool // set during shutdown; readiness then fails
}

// Route is one registered endpoint. The OpenAPI drift test reads Routes.
type Route struct {
	Method, Path string
	Auth, Idem   bool
	handler      func(a *API) http.HandlerFunc
}

// Routes lists every endpoint. Paths use Go 1.22 ServeMux patterns,
// which match the OpenAPI path templates.
var Routes = []Route{
	{"GET", "/healthz", false, false, func(a *API) http.HandlerFunc { return a.healthz }},
	{"GET", "/readyz", false, false, func(a *API) http.HandlerFunc { return a.readyz }},
	{"GET", "/metrics", false, false, func(a *API) http.HandlerFunc { return a.metrics }},
	{"POST", "/v1/card-tokens", true, true, func(a *API) http.HandlerFunc { return a.createCardToken }},
	{"POST", "/v1/payments", true, true, func(a *API) http.HandlerFunc { return a.createPayment }},
	{"GET", "/v1/payments", true, false, func(a *API) http.HandlerFunc { return a.listPayments }},
	{"GET", "/v1/payments/{id}", true, false, func(a *API) http.HandlerFunc { return a.getPayment }},
	{"POST", "/v1/payments/{id}/authorize", true, true, func(a *API) http.HandlerFunc { return a.authorize }},
	{"POST", "/v1/payments/{id}/capture", true, true, func(a *API) http.HandlerFunc { return a.capture }},
	{"POST", "/v1/payments/{id}/void", true, true, func(a *API) http.HandlerFunc { return a.void }},
	{"POST", "/v1/payments/{id}/refunds", true, true, func(a *API) http.HandlerFunc { return a.refund }},
	{"GET", "/v1/payments/{id}/events", true, false, func(a *API) http.HandlerFunc { return a.events }},
	{"GET", "/v1/payments/{id}/ledger", true, false, func(a *API) http.HandlerFunc { return a.ledger }},
	{"POST", "/v1/simulator/payments/{id}/pix-paid", true, true, func(a *API) http.HandlerFunc { return a.simulatePixPaid }},
}

// Handler builds the router.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range Routes {
		if strings.HasPrefix(rt.Path, "/v1/simulator") && !a.Simulator {
			continue
		}
		h := rt.handler(a)
		if rt.Idem {
			h = a.idempotent(h)
		}
		if rt.Auth {
			h = a.authenticate(h)
		}
		mux.HandleFunc(rt.Method+" "+rt.Path, a.observe(h))
	}
	mux.HandleFunc("/", a.observe(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, "not_found", "Resource not found", "")
	}))
	return mux
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body strictly. An empty body is allowed when
// optional is true. The error detail never repeats the body.
func decode(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "")
		return false
	}
	if optional && len(strings.TrimSpace(string(body))) == 0 {
		return true
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil || dec.More() {
		writeProblem(w, r, http.StatusBadRequest, "malformed_request", "Request body is not valid JSON for this endpoint",
			"Check field names and types. Amounts are integers in minor units (centavos).")
		return false
	}
	return true
}

// ---- probes ----

func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	if a.Draining != nil && a.Draining.Load() {
		writeProblem(w, r, http.StatusServiceUnavailable, "not_ready", "Shutting down", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := a.Store.Ping(ctx); err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "not_ready", "Database not reachable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	if a.Metrics == nil {
		writeProblem(w, r, http.StatusNotFound, "not_found", "Metrics disabled", "")
		return
	}
	promhttp.HandlerFor(a.Metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

// ---- card tokens ----

type cardTokenRequest struct {
	Number   string `json:"number"`
	CVV      string `json:"cvv"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

type cardTokenResponse struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	Brand     string    `json:"brand"`
	Last4     string    `json:"last4"`
	ExpMonth  int       `json:"exp_month"`
	ExpYear   int       `json:"exp_year"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *API) createCardToken(w http.ResponseWriter, r *http.Request) {
	var req cardTokenRequest
	if !decode(w, r, &req, false) {
		return
	}
	t, err := a.Svc.Tokenize(r.Context(), merchantFrom(r.Context()).ID, service.CardInput{
		Number: req.Number, CVV: req.CVV, ExpMonth: req.ExpMonth, ExpYear: req.ExpYear,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, cardTokenResponse{
		ID: t.Token, Object: "card_token", Brand: t.Brand, Last4: t.Last4,
		ExpMonth: t.ExpMonth, ExpYear: t.ExpYear, CreatedAt: t.CreatedAt.UTC(),
	})
}

// ---- payments ----

type createPaymentRequest struct {
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Method      string `json:"method"`
	CardToken   string `json:"card_token"`
	Description string `json:"description"`
	Reference   string `json:"reference"`
}

func (a *API) createPayment(w http.ResponseWriter, r *http.Request) {
	var req createPaymentRequest
	if !decode(w, r, &req, false) {
		return
	}
	p, err := a.Svc.CreatePayment(r.Context(), merchantFrom(r.Context()).ID, service.CreateInput{
		Amount: money.Amount(req.Amount), Currency: req.Currency, Method: req.Method,
		CardToken: req.CardToken, Description: req.Description, Reference: req.Reference,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p.View())
}

func (a *API) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := a.Svc.Get(r.Context(), merchantFrom(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.View())
}

type listResponse struct {
	Object     string `json:"object"`
	Data       any    `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func encodeCursor(p payment.Payment) string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + p.ID))
}

func decodeCursor(s string) (*store.Cursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	ts, pid, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, false
	}
	return &store.Cursor{CreatedAt: t, ID: pid}, true
}

func (a *API) listPayments(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeProblemObj(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: "validation_error",
				Title: "The request is not valid", Detail: "limit must be between 1 and 100", Field: "limit"})
			return
		}
		limit = n
	}
	var after *store.Cursor
	if v := r.URL.Query().Get("cursor"); v != "" {
		c, ok := decodeCursor(v)
		if !ok {
			writeProblemObj(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: "validation_error",
				Title: "The request is not valid", Detail: "cursor is not valid", Field: "cursor"})
			return
		}
		after = c
	}
	ps, err := a.Store.ListPayments(r.Context(), merchantFrom(r.Context()).ID, after, limit+1)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	resp := listResponse{Object: "list", Data: []payment.View{}}
	if len(ps) > limit {
		resp.HasMore = true
		ps = ps[:limit]
		resp.NextCursor = encodeCursor(ps[len(ps)-1])
	}
	views := make([]payment.View, len(ps))
	for i, p := range ps {
		views[i] = p.View()
	}
	resp.Data = views
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) authorize(w http.ResponseWriter, r *http.Request) {
	p, err := a.Svc.Authorize(r.Context(), merchantFrom(r.Context()).ID, r.PathValue("id"))
	a.respondPayment(w, r, p, err)
}

type amountRequest struct {
	Amount *int64 `json:"amount"`
}

func (req amountRequest) amount() *money.Amount {
	if req.Amount == nil {
		return nil
	}
	a := money.Amount(*req.Amount)
	return &a
}

func (a *API) capture(w http.ResponseWriter, r *http.Request) {
	var req amountRequest
	if !decode(w, r, &req, true) {
		return
	}
	p, err := a.Svc.Capture(r.Context(), merchantFrom(r.Context()).ID, r.PathValue("id"), req.amount())
	a.respondPayment(w, r, p, err)
}

func (a *API) void(w http.ResponseWriter, r *http.Request) {
	p, err := a.Svc.Void(r.Context(), merchantFrom(r.Context()).ID, r.PathValue("id"))
	a.respondPayment(w, r, p, err)
}

func (a *API) refund(w http.ResponseWriter, r *http.Request) {
	var req amountRequest
	if !decode(w, r, &req, true) {
		return
	}
	p, err := a.Svc.Refund(r.Context(), merchantFrom(r.Context()).ID, r.PathValue("id"), req.amount())
	a.respondPayment(w, r, p, err)
}

func (a *API) simulatePixPaid(w http.ResponseWriter, r *http.Request) {
	p, err := a.Svc.SimulatePixPaid(r.Context(), merchantFrom(r.Context()).ID, r.PathValue("id"))
	a.respondPayment(w, r, p, err)
}

func (a *API) respondPayment(w http.ResponseWriter, r *http.Request, p payment.Payment, err error) {
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.View())
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	a.subList(w, r, func(ctx context.Context, mid, pid string) (any, int, error) {
		ev, err := a.Store.Events(ctx, mid, pid)
		return ev, len(ev), err
	})
}

func (a *API) ledger(w http.ResponseWriter, r *http.Request) {
	a.subList(w, r, func(ctx context.Context, mid, pid string) (any, int, error) {
		le, err := a.Store.Ledger(ctx, mid, pid)
		return le, len(le), err
	})
}

// subList answers a payment sub-collection after checking that the
// payment belongs to the merchant.
func (a *API) subList(w http.ResponseWriter, r *http.Request, get func(ctx context.Context, mid, pid string) (any, int, error)) {
	mid, pid := merchantFrom(r.Context()).ID, r.PathValue("id")
	if _, err := a.Svc.Get(r.Context(), mid, pid); err != nil {
		a.writeError(w, r, err)
		return
	}
	data, n, err := get(r.Context(), mid, pid)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if n == 0 {
		data = []struct{}{}
	}
	writeJSON(w, http.StatusOK, listResponse{Object: "list", Data: data})
}
