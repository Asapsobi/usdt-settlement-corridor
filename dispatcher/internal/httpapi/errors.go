package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/slots"
)

// apiError is the one shape every error response takes: a stable
// status, a stable code string (never reused for a different
// condition), and a human-readable message that may change freely.
// Same discipline as every prior component's own apiError.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

var (
	errInvalidRequest = newAPIError(http.StatusBadRequest, "invalid_request", "the request could not be validated")
	errUnauthorized   = newAPIError(http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
	errInternal       = newAPIError(http.StatusInternalServerError, "internal", "internal error")

	errDispatchNotFound = newAPIError(http.StatusNotFound, "dispatch_not_found", "no order has ever entered dispatching with that order_id")
	errSlotNotFound     = newAPIError(http.StatusNotFound, "slot_not_found", "no slot exists with that id")
	errIllegalSlotState = newAPIError(http.StatusConflict, "illegal_slot_transition", "no such slot status transition is legal from its current status")

	// errUpstreamError covers a non-404 failure reaching C1 -- surfaced
	// distinctly from errInternal so an operator reading logs/metrics can
	// tell "C1 itself is unhappy" from "this service broke on its own".
	errUpstreamError = newAPIError(http.StatusBadGateway, "upstream_error", "C1 returned an unexpected error resolving this order")
	errOrderNotReady = newAPIError(http.StatusConflict, "order_not_ready", "the order is not in a state this operation accepts")
)

// mapError translates a domain error from any lower layer into the
// stable apiError HTTP callers see, same convention as every prior
// component's own mapError: more specific sentinels checked before more
// general ones.
func mapError(err error) *apiError {
	switch {
	case errors.Is(err, dispatch.ErrAttemptNotFound), errors.Is(err, dispatch.ErrBroadcastAttemptNotFound):
		return errDispatchNotFound
	case errors.Is(err, slots.ErrSlotNotFound):
		return errSlotNotFound
	case errors.Is(err, slots.ErrIllegalTransition):
		return errIllegalSlotState
	case errors.Is(err, slots.ErrDuplicateSlot):
		return newAPIError(http.StatusConflict, "duplicate_slot", "a slot with that id or address already exists")
	case errors.Is(err, ledgerclient.ErrOrderNotFound):
		return newAPIError(http.StatusNotFound, "order_not_found", "C1 has no order with that external_id")
	case errors.Is(err, ledgerclient.ErrIllegalTransition), errors.Is(err, ledgerclient.ErrVersionConflict):
		return errOrderNotReady

	default:
		var apiErr *ledgerclient.APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status == http.StatusNotFound {
				return newAPIError(http.StatusNotFound, "order_not_found", "C1 has no order with that external_id")
			}
			return errUpstreamError
		}
		return errInternal
	}
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: e.Code, Message: e.Message}})
}

// writeErr maps err through mapError and writes it -- the one place
// every handler's error path funnels through.
func writeErr(w http.ResponseWriter, err error) {
	writeAPIError(w, mapError(err))
}
