package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kmadsdev/simple-payment-gateway/internal/observability"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/service"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// problemBase is the base of the "type" URI. Each code has a section in
// docs/errors.md.
const problemBase = "https://github.com/kmadsdev/simple-payment-gateway/blob/main/docs/errors.md#"

// Problem is an RFC 7807 problem details object.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Code      string `json:"code"`
	Field     string `json:"field,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	writeProblemObj(w, r, Problem{Status: status, Code: code, Title: title, Detail: detail})
}

func writeProblemObj(w http.ResponseWriter, r *http.Request, p Problem) {
	p.Type = problemBase + p.Code
	p.Instance = r.URL.Path
	p.RequestID = observability.RequestID(r.Context())
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// writeError maps service and domain errors to problems.
func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var verr *service.ValidationError
	var terr *payment.TransitionError
	switch {
	case errors.As(err, &verr):
		writeProblemObj(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: "validation_error",
			Title: "The request is not valid", Detail: verr.Error(), Field: verr.Field})
	case errors.As(err, &terr):
		writeProblem(w, r, http.StatusConflict, "invalid_state_transition", "Operation not allowed in the current state", terr.Error())
	case errors.Is(err, payment.ErrAmountTooHigh):
		writeProblem(w, r, http.StatusUnprocessableEntity, "amount_too_high", "Amount above the allowed limit", err.Error())
	case errors.Is(err, payment.ErrInvalidAmount):
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_amount", "Amount must be a positive integer in minor units", "")
	case errors.Is(err, payment.ErrWrongMethod):
		writeProblem(w, r, http.StatusConflict, "method_not_supported", "Operation not supported for this payment method", "")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, service.ErrSimulatorDisabled):
		writeProblem(w, r, http.StatusNotFound, "not_found", "Resource not found", "")
	case errors.Is(err, store.ErrOperationInProgress):
		writeProblem(w, r, http.StatusConflict, "operation_in_progress", "Another operation on this payment is in progress", "Retry after it finishes.")
	case errors.Is(err, store.ErrVersionConflict):
		writeProblem(w, r, http.StatusConflict, "concurrent_update", "The payment changed during the request", "Read it again and retry.")
	case errors.Is(err, service.ErrProviderUnavailable):
		w.Header().Set("Retry-After", "5")
		writeProblem(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Payment provider unavailable",
			"The payment did not change. Retry later.")
	default:
		a.Log.ErrorContext(r.Context(), "unhandled error", "error", err.Error())
		writeProblem(w, r, http.StatusInternalServerError, "internal_error", "Internal error", "")
	}
}
